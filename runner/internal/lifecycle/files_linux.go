package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// boundary is / in production. Tests may restrict this to their private tree.
type protectedFiles struct {
	uid      uint32
	boundary string
	syncHook func(*os.File) error
}

func (p protectedFiles) syncFile(f *os.File) error {
	if p.syncHook != nil {
		return p.syncHook(f)
	}
	return f.Sync()
}

func openPath(path string, flags int, mode uint32) (*os.File, error) {
	if flags&unix.O_PATH == 0 {
		flags |= unix.O_NONBLOCK
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW), Mode: uint64(mode), Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func metadata(f *os.File, uid, mode uint32) error {
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Uid != uid || st.Mode != mode || mode&unix.S_IFMT == unix.S_IFREG && st.Nlink != 1 {
		return errRefused
	}
	return nil
}

func samePath(f *os.File, path string) bool {
	current, err := openPath(path, unix.O_PATH, 0)
	if err != nil {
		return false
	}
	defer current.Close()
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := current.Stat()
	return err == nil && os.SameFile(a, b)
}

func (p protectedFiles) parents(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !filepath.IsAbs(p.boundary) || (path != p.boundary && !strings.HasPrefix(path, strings.TrimSuffix(p.boundary, "/")+"/")) {
		return errRefused
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		f, err := openPath(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		var st unix.Stat_t
		err = unix.Fstat(int(f.Fd()), &st)
		valid := err == nil && st.Uid == p.uid && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&0022 == 0 && samePath(f, dir)
		f.Close()
		if !valid {
			return errRefused
		}
		if dir == p.boundary {
			return nil
		}
		if dir == "/" {
			return errRefused
		}
	}
}

func (p protectedFiles) read(path string, mode uint32, limit int64) ([]byte, error) {
	return p.readFile(path, mode, limit, false)
}

func (p protectedFiles) readFile(path string, mode uint32, limit int64, durable bool) ([]byte, error) {
	if p.parents(path) != nil {
		return nil, errRefused
	}
	f, err := openPath(path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if metadata(f, p.uid, unix.S_IFREG|mode) != nil {
		return nil, errRefused
	}
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errRefused
	}
	if durable {
		if err := p.syncFile(f); err != nil {
			return nil, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		again, err := io.ReadAll(io.LimitReader(f, limit+1))
		if err != nil || !bytes.Equal(again, raw) {
			return nil, errRefused
		}
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !samePath(f, path) || metadata(f, p.uid, unix.S_IFREG|mode) != nil || p.parents(path) != nil {
		return nil, errRefused
	}
	return raw, nil
}

type operation struct {
	files       protectedFiles
	parent, dir *os.File
	path        string
	request     []byte
}

func (p protectedFiles) reserve(parent, id string, raw []byte) (*operation, error) {
	if p.parents(filepath.Join(parent, "unused")) != nil {
		return nil, errRefused
	}
	f, err := openPath(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	if metadata(f, p.uid, unix.S_IFDIR|0700) != nil {
		f.Close()
		return nil, errRefused
	}
	if err := unix.Mkdirat(int(f.Fd()), id, 0700); err != nil {
		f.Close()
		return nil, err
	}
	// A partial directory is deliberately retained, including on sync failure.
	if err := p.syncFile(f); err != nil {
		f.Close()
		return nil, err
	}
	path := filepath.Join(parent, id)
	dir, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		f.Close()
		return nil, err
	}
	o := &operation{p, f, dir, path, append([]byte(nil), raw...)}
	if metadata(dir, p.uid, unix.S_IFDIR|0700) != nil || !samePath(f, parent) {
		o.close()
		return nil, errRefused
	}
	if err := o.create("request.json", raw); err != nil {
		o.close()
		return nil, err
	}
	if err := p.syncFile(dir); err != nil {
		o.close()
		return nil, err
	}
	current, err := p.read(filepath.Join(path, "request.json"), 0600, 64*1024)
	if err != nil || !bytes.Equal(current, raw) {
		o.close()
		return nil, errRefused
	}
	return o, nil
}

func (o *operation) close() { o.dir.Close(); o.parent.Close() }

func (o *operation) create(name string, raw []byte) error {
	fd, err := unix.Openat(int(o.dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if metadata(f, o.files.uid, unix.S_IFREG|0600) != nil {
		return errRefused
	}
	if n, err := f.Write(raw); err != nil {
		return err
	} else if n != len(raw) {
		return io.ErrShortWrite
	}
	if err := o.files.syncFile(f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	current, err := io.ReadAll(io.LimitReader(f, 128*1024+1))
	if err != nil || !bytes.Equal(current, raw) || !samePath(f, filepath.Join(o.path, name)) || metadata(f, o.files.uid, unix.S_IFREG|0600) != nil {
		return errRefused
	}
	return nil
}

func (o *operation) unchanged() bool {
	if !samePath(o.dir, o.path) || !samePath(o.parent, filepath.Dir(o.path)) || metadata(o.dir, o.files.uid, unix.S_IFDIR|0700) != nil || metadata(o.parent, o.files.uid, unix.S_IFDIR|0700) != nil {
		return false
	}
	raw, err := o.files.read(filepath.Join(o.path, "request.json"), 0600, 64*1024)
	return err == nil && bytes.Equal(raw, o.request)
}

func (o *operation) commit(ctx context.Context, data []byte, recheck func() error) error {
	if len(data) > 128*1024 || ctx.Err() != nil || !o.unchanged() || recheck() != nil {
		return errRefused
	}
	if err := o.create("execution.json", data); err != nil {
		return err
	}
	if err := o.files.syncFile(o.dir); err != nil {
		return err
	}
	raw, err := o.files.read(filepath.Join(o.path, "execution.json"), 0600, 128*1024)
	if err != nil || !bytes.Equal(raw, data) || !o.unchanged() || ctx.Err() != nil || recheck() != nil {
		return errRefused
	}
	complete, _ := json.Marshal(completion{1, fmt.Sprintf("%x", sha256.Sum256(data))})
	// The completion seal is the publication boundary. execution.json alone is
	// a retained partial attempt and can never satisfy original-result readback.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := o.create("complete.json", append(complete, '\n')); err != nil {
		return err
	}
	if err := o.files.syncFile(o.dir); err != nil {
		return err
	}
	return nil
}

type completion struct {
	Version      int    `json:"version"`
	ExecutionSHA string `json:"execution_sha256"`
}

func (p protectedFiles) original(parent string, r request, requestRaw []byte, producerSHA string) (bool, error) {
	path := filepath.Join(parent, r.Operation)
	if p.parents(filepath.Join(parent, "unused")) != nil {
		return false, errRefused
	}
	parentFile, err := openPath(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return false, err
	}
	defer parentFile.Close()
	if metadata(parentFile, p.uid, unix.S_IFDIR|0700) != nil {
		return false, errRefused
	}
	dir, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer dir.Close()
	if metadata(dir, p.uid, unix.S_IFDIR|0700) != nil {
		return false, errRefused
	}
	entries, err := dir.ReadDir(-1)
	if err != nil || len(entries) != 3 {
		return false, errRefused
	}
	storedRequest, err := p.readFile(filepath.Join(path, "request.json"), 0600, 64*1024, true)
	if err != nil || !bytes.Equal(storedRequest, requestRaw) {
		return false, errRefused
	}
	data, err := p.readFile(filepath.Join(path, "execution.json"), 0600, 128*1024, true)
	if err != nil {
		return false, err
	}
	seal, err := p.readFile(filepath.Join(path, "complete.json"), 0600, 4096, true)
	if err != nil {
		return false, err
	}
	var complete completion
	var record receipt
	if strictObject(seal, &complete) != nil || complete.Version != 1 || complete.ExecutionSHA != fmt.Sprintf("%x", sha256.Sum256(data)) || strictObject(data, &record) != nil || record.Version != 1 || record.ProducerSHA != producerSHA || record.RequestSHA != fmt.Sprintf("%x", sha256.Sum256(requestRaw)) || record.Expected != r {
		return false, errRefused
	}
	b := record.Observed.Guard
	vm := record.Observed.VM
	if b.Controller != r.Controller || b.Repository != r.GARMRepository || b.Pool != b.ScaleSet+"-"+b.Repository || !uuidPattern.MatchString(b.UUID) || b.AgentID <= 0 || record.Observed.RunnerID != b.AgentID || record.Observed.RunnerName != b.Name || record.Observed.GitHubScaleSetID <= 0 || vm.PID != record.Observed.Host.PID || !record.Observed.Host.valid() || !uuidPattern.MatchString(vm.VolumeUUID) || vm.VolumeUUID == b.UUID || vm.VolumeName != b.Name || vm.VolumeType != "virtual-machine" || vm.VolumePool != "halo-ci" || vm.VolumeProject != "halo-ci" || vm.VolumeContent != "block" {
		return false, errRefused
	}
	for _, s := range []string{b.StateSHA, b.PolicySHA, b.ProviderSHA, b.ConfigSHA, b.Image} {
		if !digestPattern.MatchString(s) {
			return false, errRefused
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, record.CapturedAt); err != nil {
		return false, errRefused
	}
	if !samePath(dir, path) {
		return false, errRefused
	}
	if p.syncFile(dir) != nil || p.syncFile(parentFile) != nil || !samePath(dir, path) || !samePath(parentFile, parent) {
		return false, errRefused
	}
	for _, file := range []struct {
		name  string
		raw   []byte
		limit int64
	}{{"request.json", storedRequest, 64 * 1024}, {"execution.json", data, 128 * 1024}, {"complete.json", seal, 4096}} {
		again, err := p.read(filepath.Join(path, file.name), 0600, file.limit)
		if err != nil || !bytes.Equal(again, file.raw) {
			return false, errRefused
		}
	}
	return true, nil
}

func runningDigest() (string, error) {
	// This intentional procfs magic link identifies this process's actual image,
	// including after a deployment rename. Never derive provenance from env.
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", err
	}
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Uid != 0 || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 {
		return "", errRefused
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, 128*1024*1024+1))
	if err != nil || n > 128*1024*1024 {
		return "", errRefused
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
