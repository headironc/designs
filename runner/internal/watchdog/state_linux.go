package watchdog

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func readProtected(path string, uid uint32, mode os.FileMode, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := checkFile(f, uid, mode); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errRefused
	}
	return data, nil
}

func checkFile(f *os.File, uid uint32, mode os.FileMode) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != mode || s.Uid != uid || s.Nlink != 1 {
		return errRefused
	}
	return nil
}

func (o *observer) lock() (*os.File, error) {
	if err := os.Mkdir(o.dir, 0700); err == nil {
		if err := syncDir(filepath.Dir(o.dir)); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	dir, err := os.OpenFile(o.dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := dir.Stat()
	dir.Close()
	if err != nil {
		return nil, err
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != o.uid || info.Mode().Perm() != 0700 {
		return nil, errRefused
	}
	f, err := os.OpenFile(filepath.Join(o.dir, "operation.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	if err := checkFile(f, o.uid, 0600); err != nil {
		f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errBusy
		}
		return nil, err
	}
	return f, nil
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, w := f.Write(data)
	return errors.Join(w, f.Sync(), f.Close())
}

func atomicJSON(dir, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, w := f.Write(data)
	if err := errors.Join(w, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
