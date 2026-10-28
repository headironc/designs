package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"golang.org/x/sys/unix"
)

const pauseParent = "/var/lib/nucleiron-watchdog"
const pauseTimer = "nucleiron-provider-watchdog.timer"
const pauseWorker = "nucleiron-provider-watchdog.service"

type pausePID struct {
	PID   uint32 `json:"pid"`
	Start uint64 `json:"starttime_ticks"`
}
type pauseUnits struct {
	TimerActive     string   `json:"timer_active"`
	TimerSub        string   `json:"timer_sub"`
	TimerInvocation string   `json:"timer_invocation"`
	TimerJob        uint32   `json:"timer_job"`
	WorkerActive    string   `json:"worker_active"`
	WorkerSub       string   `json:"worker_sub"`
	Invocation      string   `json:"worker_invocation"`
	StartUSec       uint64   `json:"worker_start_usec"`
	Result          string   `json:"worker_result"`
	Main            pausePID `json:"main"`
	Control         pausePID `json:"control"`
	Group           string   `json:"cgroup"`
	GroupDevice     uint64   `json:"cgroup_device"`
	GroupInode      uint64   `json:"cgroup_inode"`
	Jobs            []uint32 `json:"worker_jobs"`
	Configuration   string   `json:"configuration_sha256"`
}
type pauseIntent struct {
	Version      int        `json:"version"`
	Operation    string     `json:"operation_id"`
	ExecutionSHA string     `json:"execution_sha256"`
	CaptureSHA   string     `json:"capture_producer_sha256"`
	ProducerSHA  string     `json:"pause_producer_sha256"`
	Boot         string     `json:"boot_id"`
	Manager      string     `json:"manager"`
	Timer        string     `json:"timer"`
	Worker       string     `json:"worker"`
	AdmissionSHA string     `json:"admission_sha256"`
	Before       pauseUnits `json:"before"`
}
type pauseJob struct {
	IntentSHA string `json:"intent_sha256"`
	ID        uint32 `json:"id"`
	Path      string `json:"path"`
}
type pauseEvent struct {
	IntentSHA string `json:"intent_sha256"`
	ID        uint32 `json:"id"`
	Path      string `json:"path"`
	Unit      string `json:"unit"`
	Sender    string `json:"sender"`
	Result    string `json:"result"`
}
type pauseDrain struct {
	IntentSHA string     `json:"intent_sha256"`
	Job       uint32     `json:"job"`
	Facts     pauseUnits `json:"facts"`
}
type pauseRecords struct {
	Intent    pauseIntent
	IntentSHA string
	Job       *pauseJob
	Event     *pauseEvent
	Drain     *pauseDrain
}

type pauseStore struct {
	files             protectedFiles
	parent, lock, dir *os.File
	path              string
	raw               map[string][]byte
	pinned            map[string]*os.File
}

func openPauseStore(files protectedFiles, parent string, createLock bool) (*pauseStore, error) {
	if files.parents(filepath.Join(parent, "unused")) != nil {
		return nil, errRefused
	}
	dir, err := openPath(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	s := &pauseStore{files: files, parent: dir, path: filepath.Join(parent, "pause"), raw: map[string][]byte{}, pinned: map[string]*os.File{}}
	fail := func() (*pauseStore, error) { s.close(); return nil, errRefused }
	if metadata(dir, files.uid, unix.S_IFDIR|0700) != nil {
		return fail()
	}
	flags, mode := unix.O_RDONLY, uint32(0)
	var slot unix.Stat_t
	statErr := unix.Fstatat(int(dir.Fd()), "pause", &slot, unix.AT_SYMLINK_NOFOLLOW)
	if statErr != nil && !errors.Is(statErr, unix.ENOENT) {
		return fail()
	}
	occupied := statErr == nil
	if createLock && occupied {
		createLock = false
	}
	if createLock {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_EXCL
		mode = 0600
	}
	s.lock, err = openPath(filepath.Join(parent, "pause.lock"), flags, mode)
	if createLock && errors.Is(err, os.ErrExist) {
		s.lock, err = openPath(filepath.Join(parent, "pause.lock"), unix.O_RDWR, 0)
	}
	if err != nil {
		s.close()
		return nil, err
	}
	if metadata(s.lock, files.uid, unix.S_IFREG|0600) != nil || unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil || files.syncFile(s.lock) != nil || files.syncFile(s.parent) != nil || !s.unchanged() {
		return fail()
	}
	return s, nil
}
func (s *pauseStore) close() {
	for _, f := range s.pinned {
		f.Close()
	}
	if s.dir != nil {
		s.dir.Close()
	}
	if s.lock != nil {
		s.lock.Close()
	}
	if s.parent != nil {
		s.parent.Close()
	}
}
func (s *pauseStore) unchanged() bool {
	info, err := s.lock.Stat()
	for name, raw := range s.raw {
		current, e := s.files.read(filepath.Join(s.path, name), 0600, 64*1024)
		if e != nil || !bytes.Equal(current, raw) || !samePath(s.pinned[name], filepath.Join(s.path, name)) || metadata(s.pinned[name], s.files.uid, unix.S_IFREG|0600) != nil {
			return false
		}
	}
	return err == nil && info.Size() == 0 && s.files.parents(s.path) == nil && samePath(s.parent, filepath.Dir(s.path)) && samePath(s.lock, filepath.Join(filepath.Dir(s.path), "pause.lock")) && metadata(s.parent, s.files.uid, unix.S_IFDIR|0700) == nil && metadata(s.lock, s.files.uid, unix.S_IFREG|0600) == nil && (s.dir == nil || samePath(s.dir, s.path) && metadata(s.dir, s.files.uid, unix.S_IFDIR|0700) == nil)
}
func (s *pauseStore) track(name string, raw []byte) error {
	if old, ok := s.raw[name]; ok {
		if !bytes.Equal(old, raw) || !s.unchanged() {
			return errRefused
		}
		return nil
	}
	f, err := openPath(filepath.Join(s.path, name), unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	s.pinned[name] = f
	s.raw[name] = append([]byte(nil), raw...)
	if !s.unchanged() {
		return errRefused
	}
	return nil
}
func (s *pauseStore) existing() (bool, error) {
	dir, err := openPath(s.path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.dir = dir
	if !s.unchanged() {
		return true, errRefused
	}
	return true, nil
}
func (s *pauseStore) reserve() error {
	if !s.unchanged() || unix.Mkdirat(int(s.parent.Fd()), "pause", 0700) != nil {
		return errRefused
	}
	if s.files.syncFile(s.parent) != nil {
		return errRefused
	}
	dir, err := openPath(s.path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	s.dir = dir
	if !s.unchanged() {
		return errRefused
	}
	return nil
}
func (s *pauseStore) create(name string, value any) error {
	if !s.unchanged() || s.dir == nil {
		return errRefused
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 64*1024 {
		return errRefused
	}
	raw = append(raw, '\n')
	// Reuse protected exclusive persistence, not capture's three-file ownership.
	o := operation{files: s.files, parent: s.parent, dir: s.dir, path: s.path}
	if err = o.create(name, raw); err != nil {
		return err
	}
	if s.files.syncFile(s.dir) != nil || !s.unchanged() {
		return errRefused
	}
	return s.track(name, raw)
}
func (s *pauseStore) load(r receipt, executionSHA, producer string) (pauseRecords, error) {
	out := pauseRecords{}
	if s.dir == nil || !s.unchanged() {
		return out, errRefused
	}
	entries, err := os.ReadDir(s.path)
	if err != nil || len(entries) > 4 {
		return out, errRefused
	}
	present := map[string]bool{}
	for _, e := range entries {
		switch e.Name() {
		case "intent.json", "job.json", "event.json", "drain.json":
			present[e.Name()] = true
		default:
			return out, errRefused
		}
	}
	if !present["intent.json"] || present["event.json"] && !present["job.json"] || present["drain.json"] && !present["event.json"] {
		return out, errRefused
	}
	read := func(name string, target any) ([]byte, error) {
		raw, err := s.files.readFile(filepath.Join(s.path, name), 0600, 64*1024, true)
		if err != nil || decodePauseRecord(raw, target) != nil || s.track(name, raw) != nil {
			return nil, errRefused
		}
		return raw, nil
	}
	raw, err := read("intent.json", &out.Intent)
	i := out.Intent
	if err != nil || i.Version != 1 || managerOwner(i.Manager) == "" || i.Operation != r.Expected.Operation || i.ExecutionSHA != executionSHA || i.CaptureSHA != r.ProducerSHA || i.ProducerSHA != producer || !digestPattern.MatchString(producer) || i.Boot != r.Observed.Host.BootID || i.Timer != pauseTimer || i.Worker != pauseWorker || !digestPattern.MatchString(i.AdmissionSHA) || !pauseInitialShape(i.Before) {
		return out, errRefused
	}
	out.IntentSHA = fmt.Sprintf("%x", sha256.Sum256(raw))
	if present["job.json"] {
		out.Job = &pauseJob{}
		if _, err = read("job.json", out.Job); err != nil || out.Job.IntentSHA != out.IntentSHA || out.Job.ID == 0 || out.Job.Path != pauseJobPath(out.Job.ID) {
			return out, errRefused
		}
	}
	if present["event.json"] {
		out.Event = &pauseEvent{}
		if _, err = read("event.json", out.Event); err != nil || out.Event.IntentSHA != out.IntentSHA || out.Event.ID != out.Job.ID || out.Event.Path != out.Job.Path || out.Event.Unit != pauseTimer || out.Event.Sender != managerOwner(i.Manager) || !pauseJobResult(out.Event.Result) {
			return out, errRefused
		}
	}
	if present["drain.json"] {
		out.Drain = &pauseDrain{}
		if _, err = read("drain.json", out.Drain); err != nil || out.Drain.IntentSHA != out.IntentSHA || out.Drain.Job != out.Job.ID || out.Event.Result != "done" || !validPauseUnits(out.Drain.Facts) || !pauseDrainedShape(out.Drain.Facts) || pauseCompatible(i.Before, out.Drain.Facts) != nil {
			return out, errRefused
		}
	}
	if s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil || !s.unchanged() {
		return out, errRefused
	}
	// Confirm immutable file bytes after the durability boundary.
	again, err := s.files.read(filepath.Join(s.path, "intent.json"), 0600, 64*1024)
	if err != nil || !bytes.Equal(raw, again) {
		return out, errRefused
	}
	return out, nil
}

func decodePauseRecord(raw []byte, target any) error {
	if strictObject(raw, target) != nil {
		return errRefused
	}
	// This producer emits every field, including zero PIDs and empty invocation
	// strings. Missing or null fields cannot reconstruct those observed facts.
	encoded, err := json.Marshal(target)
	if err != nil {
		return errRefused
	}
	var original, restored any
	a, b := json.NewDecoder(bytes.NewReader(raw)), json.NewDecoder(bytes.NewReader(encoded))
	a.UseNumber()
	b.UseNumber()
	if a.Decode(&original) != nil || b.Decode(&restored) != nil || !reflect.DeepEqual(original, restored) {
		return errRefused
	}
	return nil
}
