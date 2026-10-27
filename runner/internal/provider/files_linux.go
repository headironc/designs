package provider

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func openPath(path string, flags int) (*os.File, error) {
	// Reject symlinks in every component, including parent directories. Linux 5.6+ is required.
	flags |= unix.O_CLOEXEC | unix.O_NOFOLLOW
	if flags&unix.O_PATH == 0 {
		flags |= unix.O_NONBLOCK
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   uint64(flags),
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openAt(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func metadata(f *os.File, uid, mode uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Uid != uid || st.Mode != mode || (mode&unix.S_IFMT == unix.S_IFREG && st.Nlink != 1) {
		return errRefused
	}
	return nil
}

func readPath(path string, uid, mode uint32, limit int64) ([]byte, error) {
	f, err := openPath(path, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := readFile(f, uid, mode, limit)
	if err != nil || !samePath(f, path) {
		return nil, errRefused
	}
	return raw, nil
}

func readFile(f *os.File, uid, mode uint32, limit int64) ([]byte, error) {
	if err := metadata(f, uid, unix.S_IFREG|mode); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errRefused
	}
	return raw, nil
}

func samePath(f *os.File, path string) bool {
	current, err := openPath(path, unix.O_RDONLY|unix.O_PATH)
	if err != nil {
		return false
	}
	defer current.Close()
	return sameFile(f, current)
}

func sameAt(f, dir *os.File, name string) bool {
	current, err := openAt(dir, name)
	if err != nil {
		return false
	}
	defer current.Close()
	return sameFile(f, current)
}

func sameFile(a, b *os.File) bool {
	x, xerr := a.Stat()
	y, yerr := b.Stat()
	return xerr == nil && yerr == nil && os.SameFile(x, y)
}

func absent(path string) error {
	// Lstat observes even dangling symlinks; any error other than absence refuses startup.
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errRefused
}
