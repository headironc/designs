package firewall

import (
	"bytes"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

var errState = errors.New("firewall operation state is incomplete, changed, or unprotected")

type retainedFile struct {
	file *os.File
	raw  []byte
}

// firewallFiles owns this capability's permanent lock and immutable active
// records. It does not decide to send or retry any kernel operation.
type firewallFiles struct {
	path                 string
	uid                  uint32
	parent, lock, active *os.File
	retained             map[string]retainedFile
	sync                 func(*os.File) error
}

func protectedPath(path string, flags int) (*os.File, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK),
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func protectedAt(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func protectedMetadata(file *os.File, uid, mode uint32) bool {
	var st unix.Stat_t
	return unix.Fstat(int(file.Fd()), &st) == nil && st.Uid == uid && st.Mode == mode && (mode&unix.S_IFMT != unix.S_IFREG || st.Nlink == 1)
}

func sameOpenFile(a, b *os.File) bool {
	x, err := a.Stat()
	if err != nil {
		return false
	}
	y, err := b.Stat()
	return err == nil && os.SameFile(x, y)
}

func directoryEntries(dir *os.File) ([]os.DirEntry, error) {
	reader, err := protectedAt(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return reader.ReadDir(-1)
}

func openFirewallFiles(path string, uid uint32) (*firewallFiles, error) {
	dir, err := protectedPath(path, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	s := &firewallFiles{path: path, uid: uid, parent: dir, retained: make(map[string]retainedFile), sync: func(f *os.File) error { return f.Sync() }}
	fail := func(err error) (*firewallFiles, error) { s.Close(); return nil, err }
	if !protectedMetadata(dir, uid, unix.S_IFDIR|0700) {
		return fail(errState)
	}
	s.lock, err = protectedAt(dir, "operation.lock", unix.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		entries, e := directoryEntries(dir)
		// Any original record, including archives or an empty active slot,
		// prohibits creating a replacement permanent lock.
		if e != nil || len(entries) != 0 {
			return fail(errState)
		}
		s.lock, err = protectedAt(dir, "operation.lock", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			s.lock, err = protectedAt(dir, "operation.lock", unix.O_RDWR, 0)
		}
	}
	if err != nil {
		return fail(err)
	}
	if !protectedMetadata(s.lock, uid, unix.S_IFREG|0600) {
		return fail(errState)
	}
	if err := unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(err)
	}
	if err := s.sync(s.lock); err != nil {
		return fail(err)
	}
	if err := s.sync(s.parent); err != nil {
		return fail(err)
	}
	if !s.unchanged() {
		return fail(errState)
	}
	return s, nil
}

func (s *firewallFiles) Close() {
	for _, retained := range s.retained {
		_ = retained.file.Close()
	}
	if s.active != nil {
		_ = s.active.Close()
	}
	if s.lock != nil {
		_ = s.lock.Close()
	}
	if s.parent != nil {
		_ = s.parent.Close()
	}
}

func (s *firewallFiles) unchanged() bool {
	if s.parent == nil || s.lock == nil || !protectedMetadata(s.parent, s.uid, unix.S_IFDIR|0700) || !protectedMetadata(s.lock, s.uid, unix.S_IFREG|0600) {
		return false
	}
	current, err := protectedPath(s.path, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return false
	}
	defer current.Close()
	if !sameOpenFile(s.parent, current) {
		return false
	}
	lock, err := protectedAt(s.parent, "operation.lock", unix.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer lock.Close()
	info, err := s.lock.Stat()
	if err != nil || info.Size() != 0 || !sameOpenFile(s.lock, lock) {
		return false
	}
	if s.active != nil {
		active, err := protectedAt(s.parent, "active", unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return false
		}
		defer active.Close()
		if !sameOpenFile(s.active, active) || !protectedMetadata(s.active, s.uid, unix.S_IFDIR|0700) {
			return false
		}
	}
	for name, retained := range s.retained {
		f, err := protectedAt(s.active, name, unix.O_RDONLY, 0)
		if err != nil {
			return false
		}
		raw, e := protectedBytes(f, s.uid, 64*1024)
		same := sameOpenFile(f, retained.file)
		_ = f.Close()
		if e != nil || !same || !bytes.Equal(raw, retained.raw) {
			return false
		}
	}
	return true
}

func protectedBytes(file *os.File, uid uint32, limit int64) ([]byte, error) {
	if !protectedMetadata(file, uid, unix.S_IFREG|0600) {
		return nil, errState
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit || !protectedMetadata(file, uid, unix.S_IFREG|0600) {
		return nil, errState
	}
	return raw, nil
}

func (s *firewallFiles) openActive() (bool, error) {
	if !s.unchanged() {
		return false, errState
	}
	if s.active != nil {
		return true, nil
	}
	f, err := protectedAt(s.parent, "active", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.active = f
	if !s.unchanged() {
		return true, errState
	}
	return true, nil
}

func (s *firewallFiles) reserve() error {
	if s.active != nil || !s.unchanged() {
		return errState
	}
	if err := unix.Mkdirat(int(s.parent.Fd()), "active", 0700); err != nil {
		return err
	}
	if err := s.sync(s.parent); err != nil {
		return err
	}
	exists, err := s.openActive()
	if err != nil {
		return err
	}
	if !exists {
		return errState
	}
	return nil
}

func recordName(name string) bool {
	return name == "intent.json" || name == "result.json"
}

func (s *firewallFiles) read(name string) ([]byte, error) {
	if !recordName(name) || s.active == nil || !s.unchanged() {
		return nil, errState
	}
	if retained, ok := s.retained[name]; ok {
		return append([]byte(nil), retained.raw...), nil
	}
	f, err := protectedAt(s.active, name, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	raw, err := protectedBytes(f, s.uid, 64*1024)
	if err != nil || len(raw) == 0 {
		_ = f.Close()
		return nil, errors.Join(errState, err)
	}
	s.retained[name] = retainedFile{file: f, raw: append([]byte(nil), raw...)}
	if !s.unchanged() {
		return nil, errState
	}
	return raw, nil
}

func (s *firewallFiles) create(name string, raw []byte) error {
	if !recordName(name) || s.active == nil || len(raw) == 0 || len(raw) > 64*1024 || !s.unchanged() {
		return errState
	}
	f, err := protectedAt(s.active, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if !protectedMetadata(f, s.uid, unix.S_IFREG|0600) {
		return errState
	}
	if n, err := f.Write(raw); err != nil {
		return err
	} else if n != len(raw) {
		return io.ErrShortWrite
	}
	if err := s.sync(f); err != nil {
		return err
	}
	if err := s.sync(s.active); err != nil {
		return err
	}
	if !s.unchanged() {
		return errState
	}
	got, err := s.read(name)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, raw) {
		return errState
	}
	return nil
}

func (s *firewallFiles) archive(operation string) error {
	// Operation IDs are random 128-bit lowercase hex generated by this owner.
	if len(operation) != 32 {
		return errState
	}
	for _, c := range operation {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errState
		}
	}
	if s.active == nil || !s.unchanged() {
		return errState
	}
	if _, ok := s.retained["intent.json"]; !ok {
		return errState
	}
	if _, ok := s.retained["result.json"]; !ok {
		return errState
	}
	if err := unix.Renameat2(int(s.parent.Fd()), "active", int(s.parent.Fd()), "completed-"+operation, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := s.sync(s.parent); err != nil {
		return err
	}
	for name, retained := range s.retained {
		_ = retained.file.Close()
		delete(s.retained, name)
	}
	_ = s.active.Close()
	s.active = nil
	if !s.unchanged() {
		return errState
	}
	return nil
}
