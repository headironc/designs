package lifecycle

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sort"

	"golang.org/x/sys/unix"
)

// prepareFiles only observes existing objects. It never publishes or repairs
// a receipt, including a complete-looking file left by an interrupted writer.
type prepareFiles struct {
	protection protectedFiles
	objects    map[string]*prepareObject
}
type prepareObject struct {
	file   *os.File
	stat   unix.Stat_t
	absent bool
	names  []string
	digest *[32]byte
}

func newPrepareFiles(p protectedFiles) *prepareFiles {
	return &prepareFiles{p, map[string]*prepareObject{}}
}
func (s *prepareFiles) close() {
	for _, v := range s.objects {
		if v.file != nil {
			v.file.Close()
		}
	}
}
func samePrepareStat(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode && a.Uid == b.Uid && a.Gid == b.Gid && a.Nlink == b.Nlink && a.Size == b.Size && a.Mtim == b.Mtim && a.Ctim == b.Ctim && a.Atim == b.Atim
}
func (s *prepareFiles) open(path string, directory bool, mode uint32, body bool) (*prepareObject, error) {
	if s.protection.parents(path) != nil {
		return nil, errRefused
	}
	flags := unix.O_PATH
	if directory {
		flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOATIME
	} else if body {
		flags = unix.O_RDONLY | unix.O_NOATIME
	}
	f, err := openPath(path, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		s.objects[path] = &prepareObject{absent: true}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := &prepareObject{file: f}
	s.objects[path] = v
	if unix.Fstat(int(f.Fd()), &v.stat) != nil {
		return nil, errRefused
	}
	kind := uint32(unix.S_IFREG)
	if directory {
		kind = unix.S_IFDIR
	}
	if v.stat.Uid != s.protection.uid || v.stat.Mode != kind|mode || !directory && v.stat.Nlink != 1 || !samePath(f, path) {
		return nil, errRefused
	}
	if directory {
		v.names, err = f.Readdirnames(4097)
		if err != nil && err != io.EOF || len(v.names) > 4096 {
			return nil, errRefused
		}
		sort.Strings(v.names)
	}
	return v, nil
}
func (s *prepareFiles) read(path string, limit int64) ([]byte, error) {
	return s.readMode(path, 0600, limit)
}
func (s *prepareFiles) readMode(path string, mode uint32, limit int64) ([]byte, error) {
	v, err := s.open(path, false, mode, true)
	if err != nil || v == nil {
		return nil, err
	}
	if v.stat.Size < 0 || v.stat.Size > limit {
		return nil, errRefused
	}
	raw, err := io.ReadAll(io.LimitReader(v.file, limit+1))
	if err != nil || int64(len(raw)) > limit || int64(len(raw)) != v.stat.Size {
		return nil, errRefused
	}
	sum := sha256.Sum256(raw)
	v.digest = &sum
	return raw, nil
}
func (s *prepareFiles) unchanged(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	for path, v := range s.objects {
		if ctx.Err() != nil || s.protection.parents(path) != nil {
			return false
		}
		if v.absent {
			f, e := openPath(path, unix.O_PATH, 0)
			if f != nil {
				f.Close()
			}
			if !errors.Is(e, unix.ENOENT) {
				return false
			}
			continue
		}
		var st unix.Stat_t
		if v.file == nil || unix.Fstat(int(v.file.Fd()), &st) != nil || !samePrepareStat(v.stat, st) || !samePath(v.file, path) {
			return false
		}
		if v.names != nil {
			if _, e := v.file.Seek(0, io.SeekStart); e != nil {
				return false
			}
			names, e := v.file.Readdirnames(4097)
			if e != nil && e != io.EOF || len(names) != len(v.names) {
				return false
			}
			sort.Strings(names)
			for i := range names {
				if names[i] != v.names[i] {
					return false
				}
			}
		}
		if v.digest != nil {
			if _, e := v.file.Seek(0, io.SeekStart); e != nil {
				return false
			}
			sum := sha256.New()
			n, e := io.Copy(sum, io.LimitReader(v.file, v.stat.Size+1))
			if e != nil || n != v.stat.Size || string(sum.Sum(nil)) != string(v.digest[:]) {
				return false
			}
		}
		if unix.Fstat(int(v.file.Fd()), &st) != nil || !samePrepareStat(v.stat, st) || !samePath(v.file, path) {
			return false
		}
	}
	return ctx.Err() == nil
}

func prepareNames(v *prepareObject) map[string]bool {
	out := map[string]bool{}
	if v != nil {
		for _, name := range v.names {
			out[name] = true
		}
	}
	return out
}
