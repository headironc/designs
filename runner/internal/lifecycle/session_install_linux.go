package lifecycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type sessionConfigFiles struct {
	BeforeSHA string `json:"before_sha256"`
	AfterSHA  string `json:"after_sha256"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	GID       uint32 `json:"gid"`
}

type sessionConfigStage struct {
	IntentSHA string `json:"intent_sha256"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
}

// Only the credential window constructs this installer. Its production path
// is fixed, and stopped verifies the original stop job and drained processes.
type sessionInstaller struct {
	store   *serviceFiles
	path    string
	gid     uint32
	stopped func(context.Context) error
}

func (i sessionInstaller) read(path string, modes ...uint32) ([]byte, unix.Stat_t, error) {
	var st unix.Stat_t
	if i.store.files.parents(path) != nil {
		return nil, st, errRefused
	}
	f, err := openPath(path, unix.O_RDONLY|unix.O_NOATIME, 0)
	if err != nil {
		return nil, st, errRefused
	}
	defer f.Close()
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Gid != i.gid {
		return nil, st, errRefused
	}
	allowed := false
	for _, mode := range modes {
		allowed = allowed || metadata(f, i.store.files.uid, unix.S_IFREG|mode) == nil
	}
	if !allowed {
		return nil, st, errRefused
	}
	raw, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	var after unix.Stat_t
	attrs, xerr := unix.Flistxattr(int(f.Fd()), nil)
	if !allowed || err != nil || len(raw) > 64*1024 || int64(len(raw)) != st.Size || xerr != nil || attrs != 0 || unix.Fstat(int(f.Fd()), &after) != nil || !samePrepareStat(st, after) || !samePath(f, path) || i.store.files.parents(path) != nil {
		return nil, st, errRefused
	}
	return raw, st, nil
}

func (i sessionInstaller) present(path string) (bool, error) {
	if i.store.files.parents(path) != nil {
		return false, errRefused
	}
	f, err := openPath(path, unix.O_PATH, 0)
	if f != nil {
		f.Close()
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (i sessionInstaller) raw(name string, data []byte) error {
	s := i.store
	if !s.unchanged() {
		return errRefused
	}
	o := operation{files: s.files, parent: s.parent, dir: s.dir, path: s.path}
	if o.create(name, data) != nil || s.files.syncFile(s.dir) != nil {
		return errRefused
	}
	return s.track(name, data)
}

func (i sessionInstaller) prepare(expected string, entropy io.Reader) error {
	a := sessionAcquirer{files: i.store.files}
	if !a.absent(i.store, "configuration.json") {
		record, _, _, err := i.original()
		if err != nil || record.BeforeSHA != expected {
			return errRefused
		}
		return nil
	}
	// Partial preparation never generates another authentication domain.
	for _, name := range []string{"configuration.before.toml", "configuration.after.toml"} {
		if !a.absent(i.store, name) {
			return errRefused
		}
	}
	before, st, err := i.read(i.path, 0640)
	if err != nil || bytesSHA(before) != expected {
		return errRefused
	}
	after, err := sessionConfiguration(before, entropy)
	if err != nil || i.raw("configuration.before.toml", before) != nil || i.raw("configuration.after.toml", after) != nil {
		return errRefused
	}
	return i.store.create("configuration.json", sessionConfigFiles{bytesSHA(before), bytesSHA(after), uint64(st.Dev), st.Ino, st.Gid})
}

func (i sessionInstaller) original() (sessionConfigFiles, []byte, []byte, error) {
	a := sessionAcquirer{files: i.store.files}
	var record sessionConfigFiles
	if _, err := a.read(i.store, "configuration.json", &record); err != nil || record.GID != i.gid || record.Inode == 0 {
		return record, nil, nil, errRefused
	}
	before, err := a.read(i.store, "configuration.before.toml", nil)
	if err != nil || bytesSHA(before) != record.BeforeSHA {
		return record, nil, nil, errRefused
	}
	after, err := a.read(i.store, "configuration.after.toml", nil)
	if err != nil || bytesSHA(after) != record.AfterSHA || !validSessionConfiguration(before, after) {
		return record, nil, nil, errRefused
	}
	return record, before, after, nil
}

func (i sessionInstaller) apply(ctx context.Context) error {
	s := i.store
	a := sessionAcquirer{files: s.files}
	record, before, after, err := i.original()
	if err != nil || ctx.Err() != nil || !s.unchanged() {
		return errRefused
	}
	parent, name := filepath.Dir(i.path), filepath.Base(i.path)
	const temporary = ".garm-session-new"
	temporaryPath := filepath.Join(parent, temporary)
	dir, err := openPath(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return errRefused
	}
	defer dir.Close()
	check := func() bool {
		return ctx.Err() == nil && s.files.parents(i.path) == nil && samePath(dir, parent) && s.unchanged()
	}
	var completed, stage sessionConfigStage
	if _, e := a.read(s, "configuration.installed.json", &completed); e == nil {
		if s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil {
			return errRefused
		}
		var intended sessionConfigFiles
		intent, ie := a.read(s, "configuration.install.intent.json", &intended)
		if _, e := a.read(s, "configuration.staged.json", &stage); e != nil || ie != nil || intended != record || stage.IntentSHA != bytesSHA(intent) || completed != stage || !i.installed(after, stage) || !check() {
			return errRefused
		}
		return i.cleanup(dir, temporaryPath, before, record)
	} else if !a.absent(s, "configuration.installed.json") {
		return errRefused
	}
	if i.stopped == nil || i.stopped(ctx) != nil || !check() {
		return errRefused
	}
	if a.absent(s, "configuration.install.intent.json") {
		present, e := i.present(temporaryPath)
		if e != nil || present || s.create("configuration.install.intent.json", record) != nil {
			return errRefused
		}
	}
	var intended sessionConfigFiles
	intent, e := a.read(s, "configuration.install.intent.json", &intended)
	if e != nil || intended != record {
		return errRefused
	}
	current, currentStat, e := i.read(i.path, 0640)
	if e != nil {
		return errRefused
	}
	if bytes.Equal(current, before) && uint64(currentStat.Dev) == record.Device && currentStat.Ino == record.Inode {
		if a.absent(s, "configuration.staged.json") {
			present, e := i.present(temporaryPath)
			if e != nil {
				return errRefused
			}
			if !present {
				fd, e := unix.Openat(int(dir.Fd()), temporary, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
				if e != nil {
					return errRefused
				}
				candidate := os.NewFile(uintptr(fd), temporaryPath)
				ok := unix.Fchown(fd, int(s.files.uid), int(i.gid)) == nil && unix.Fchmod(fd, 0640) == nil
				n, we := candidate.Write(after)
				ok = ok && we == nil && n == len(after) && s.files.syncFile(candidate) == nil
				candidate.Close()
				if !ok || s.files.syncFile(dir) != nil {
					return errRefused
				}
			}
			data, st, e := i.read(temporaryPath, 0640)
			if e != nil || !bytes.Equal(data, after) || st.Dev != currentStat.Dev {
				return errRefused
			}
			stage = sessionConfigStage{bytesSHA(intent), uint64(st.Dev), st.Ino}
			if s.create("configuration.staged.json", stage) != nil {
				return errRefused
			}
		}
		if _, e := a.read(s, "configuration.staged.json", &stage); e != nil || stage.IntentSHA != bytesSHA(intent) {
			return errRefused
		}
		data, st, e := i.read(temporaryPath, 0640)
		if e != nil || !bytes.Equal(data, after) || uint64(st.Dev) != stage.Device || st.Ino != stage.Inode || i.stopped(ctx) != nil || !check() {
			return errRefused
		}
		// Exchange retains the replaced inode. An unexpected competing writer
		// leaves both objects for inspection instead of silently losing its file.
		if unix.Renameat2(int(dir.Fd()), temporary, int(dir.Fd()), name, unix.RENAME_EXCHANGE) != nil || s.files.syncFile(dir) != nil {
			return errRefused
		}
	} else if _, e := a.read(s, "configuration.staged.json", &stage); e != nil || stage.IntentSHA != bytesSHA(intent) {
		return errRefused
	}
	if !i.installed(after, stage) || i.stopped(ctx) != nil || !check() {
		return errRefused
	}
	old, st, e := i.read(temporaryPath, 0640, 0600)
	if e != nil || !bytes.Equal(old, before) || uint64(st.Dev) != record.Device || st.Ino != record.Inode {
		return errRefused
	}
	f, e := openPath(temporaryPath, unix.O_RDONLY, 0)
	if e != nil {
		return errRefused
	}
	e = unix.Fchmod(int(f.Fd()), 0600)
	if e == nil {
		e = s.files.syncFile(f)
	}
	f.Close()
	if e != nil || s.files.syncFile(dir) != nil || s.create("configuration.installed.json", stage) != nil {
		return errRefused
	}
	return i.cleanup(dir, temporaryPath, before, record)
}

func (i sessionInstaller) installed(after []byte, stage sessionConfigStage) bool {
	current, st, err := i.read(i.path, 0640)
	return err == nil && bytes.Equal(current, after) && uint64(st.Dev) == stage.Device && st.Ino == stage.Inode
}

func (i sessionInstaller) cleanup(dir *os.File, path string, before []byte, record sessionConfigFiles) error {
	s := i.store
	a := sessionAcquirer{files: s.files}
	if !a.absent(s, "configuration.cleanup.json") {
		var done sessionConfigFiles
		_, err := a.read(s, "configuration.cleanup.json", &done)
		present, e := i.present(path)
		if err != nil || done != record || e != nil || present {
			return errRefused
		}
		return nil
	}
	present, err := i.present(path)
	if err != nil {
		return errRefused
	}
	if present {
		data, st, e := i.read(path, 0600)
		if e != nil || !bytes.Equal(data, before) || uint64(st.Dev) != record.Device || st.Ino != record.Inode || !samePath(dir, filepath.Dir(path)) {
			return errRefused
		}
		if unix.Unlinkat(int(dir.Fd()), filepath.Base(path), 0) != nil {
			return errRefused
		}
	}
	if s.files.syncFile(dir) != nil || !s.unchanged() {
		return errRefused
	}
	return s.create("configuration.cleanup.json", record)
}
