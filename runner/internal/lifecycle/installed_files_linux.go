package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type installedObject struct {
	path    string
	file    *os.File
	stat    unix.Stat_t
	absent  bool
	digest  string
	xattrs  bool
	parents []string
}

// This observer owns installed-file facts, not the protected receipt format.
// Parent descriptors are retained because the historical owner lock does not
// serialize administrators replacing installed files or their ancestors.
type installedFiles struct {
	protection protectedFiles
	parents    map[string]*installedObject
	objects    []*installedObject
}

func (s *installedFiles) close() {
	for _, p := range s.parents {
		p.file.Close()
	}
	for _, p := range s.objects {
		if p.file != nil {
			p.file.Close()
		}
	}
}

func (s *installedFiles) parentPaths(path string) ([]string, error) {
	if s.protection.parents(path) != nil {
		return nil, errRefused
	}
	paths := []string{}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		paths = append(paths, dir)
		if _, exists := s.parents[dir]; !exists {
			f, err := openPath(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
			if err != nil {
				return nil, errRefused
			}
			var st unix.Stat_t
			if unix.Fstat(int(f.Fd()), &st) != nil || st.Uid != s.protection.uid || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0022 != 0 || !samePath(f, dir) {
				f.Close()
				return nil, errRefused
			}
			s.parents[dir] = &installedObject{path: dir, file: f, stat: st}
		}
		if dir == s.protection.boundary {
			return paths, nil
		}
	}
}

func installedHash(ctx context.Context, f *os.File, expected int64) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", errRefused
	}
	h := sha256.New()
	buf := make([]byte, 32*1024)
	var total int64
	for ctx.Err() == nil {
		limit := len(buf)
		if remaining := expected - total; remaining < int64(limit) {
			// At most one byte beyond the expected size; no size+1 overflow.
			limit = int(remaining) + 1
		}
		n, err := f.Read(buf[:limit])
		if int64(n) > expected-total {
			return "", errRefused
		}
		total += int64(n)
		h.Write(buf[:n])
		if err == io.EOF {
			if total != expected {
				return "", errRefused
			}
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil || n == 0 {
			return "", errRefused
		}
	}
	return "", errRefused
}

func installedXattrs(f *os.File) (bool, error) {
	// The pinned plan only permits SHA256("{}"). Names and values are neither
	// fetched nor emitted: a nonempty set already establishes a mismatch.
	n, err := unix.Flistxattr(int(f.Fd()), nil)
	return n != 0, err
}

func (s *installedFiles) observe(ctx context.Context, path string, expected prepareFact, beforeBody func()) prepareStatus {
	parents, err := s.parentPaths(path)
	if err != nil {
		return prepareUnknown("parent_untrusted")
	}
	v := &installedObject{path: path, parents: parents}
	f, err := openPath(path, unix.O_PATH, 0)
	if errors.Is(err, unix.ENOENT) {
		v.absent = true
		s.objects = append(s.objects, v)
		return prepareStatus{"missing", "file_missing"}
	}
	if err != nil {
		return prepareUnknown("file_unavailable")
	}
	v.file = f
	s.objects = append(s.objects, v)
	if unix.Fstat(int(f.Fd()), &v.stat) != nil || !samePath(f, path) {
		return prepareUnknown("file_identity_unavailable")
	}
	if v.stat.Mode&unix.S_IFMT != unix.S_IFREG || v.stat.Nlink != 1 {
		return prepareUnknown("file_unsafe")
	}
	for _, check := range []struct {
		wrong bool
		code  string
	}{{v.stat.Uid != *expected.UID, "file_uid"}, {v.stat.Gid != *expected.GID, "file_gid"}, {v.stat.Mode&07777 != *expected.Mode, "file_mode"}, {v.stat.Size != *expected.Size, "file_size"}} {
		if check.wrong {
			return prepareStatus{"mismatch", check.code}
		}
	}
	if beforeBody != nil {
		beforeBody()
	}
	// openPath adds O_NONBLOCK. A replacement FIFO cannot block this open;
	// its identity/type is rejected before any read, even after O_PATH passed.
	body, err := openPath(path, unix.O_RDONLY|unix.O_NOATIME, 0)
	if err != nil {
		return prepareUnknown("file_body_unavailable")
	}
	var st unix.Stat_t
	if unix.Fstat(int(body.Fd()), &st) != nil || !samePrepareStat(st, v.stat) || !samePath(body, path) {
		body.Close()
		return prepareUnknown("file_changed")
	}
	f.Close()
	v.file = body
	v.xattrs, err = installedXattrs(body)
	if err != nil {
		return prepareUnknown("file_xattrs_unavailable")
	}
	v.digest, err = installedHash(ctx, body, v.stat.Size)
	if err != nil {
		return prepareUnknown("file_read_incomplete")
	}
	if v.xattrs {
		return prepareStatus{"mismatch", "file_xattrs"}
	}
	if v.digest != *expected.SHA {
		return prepareStatus{"mismatch", "file_content"}
	}
	return prepareStatus{State: "matched"}
}

func (s *installedFiles) unchanged(ctx context.Context, v *installedObject) bool {
	if ctx.Err() != nil || s.protection.parents(v.path) != nil {
		return false
	}
	for _, dir := range v.parents {
		p := s.parents[dir]
		var st unix.Stat_t
		if unix.Fstat(int(p.file.Fd()), &st) != nil || st.Dev != p.stat.Dev || st.Ino != p.stat.Ino || st.Uid != p.stat.Uid || st.Gid != p.stat.Gid || st.Mode != p.stat.Mode || !samePath(p.file, dir) {
			return false
		}
	}
	if v.absent {
		f, err := openPath(v.path, unix.O_PATH, 0)
		if f != nil {
			f.Close()
		}
		return errors.Is(err, unix.ENOENT)
	}
	var st unix.Stat_t
	if v.file == nil || unix.Fstat(int(v.file.Fd()), &st) != nil || !samePrepareStat(st, v.stat) || !samePath(v.file, v.path) {
		return false
	}
	if v.digest != "" {
		attrs, err := installedXattrs(v.file)
		if err != nil || attrs != v.xattrs {
			return false
		}
		digest, err := installedHash(ctx, v.file, v.stat.Size)
		if err != nil || digest != v.digest {
			return false
		}
	}
	return unix.Fstat(int(v.file.Fd()), &st) == nil && samePrepareStat(st, v.stat) && samePath(v.file, v.path)
}
