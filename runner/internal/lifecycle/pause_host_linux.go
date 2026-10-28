package lifecycle

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var errPauseBusy = errors.New("original worker is not drained")

type pauseHost struct {
	proc, cgroup string
	realFS       bool
}

func (h pauseHost) fs(path string, kind int64) error {
	f, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	var st unix.Statfs_t
	if unix.Fstatfs(int(f.Fd()), &st) != nil || h.realFS && int64(st.Type) != kind {
		return errRefused
	}
	return nil
}
func (h pauseHost) boot() (string, error) {
	if h.fs(h.proc, unix.PROC_SUPER_MAGIC) != nil {
		return "", errRefused
	}
	f, err := openPath(filepath.Join(h.proc, "sys/kernel/random/boot_id"), unix.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 129))
	id := strings.TrimSpace(string(raw))
	if err != nil || len(raw) > 128 || !uuidPattern.MatchString(id) {
		return "", errRefused
	}
	return id, nil
}
func (h pauseHost) process(pid uint32, expectedStart uint64) (uint64, error) {
	if pid == 0 {
		return 0, nil
	}
	if pid > 2147483647 || h.fs(h.proc, unix.PROC_SUPER_MAGIC) != nil {
		return 0, errRefused
	}
	path := filepath.Join(h.proc, strconv.FormatUint(uint64(pid), 10))
	dir, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	raw, err := readAt(dir, "stat", 8192)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, errPauseBusy
		}
		return 0, err
	}
	// Reaping states retain their identity. The drain caller must compare the
	// original starttime before deciding to wait, including for a zombie.
	start, state, err := statIdentity(raw, int64(pid))
	if err != nil {
		return 0, errRefused
	}
	if expectedStart != 0 && start != expectedStart {
		return 0, errRefused
	}
	group, err := readAt(dir, "cgroup", 4096)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, errPauseBusy
		}
		return 0, err
	}
	name, err := groupPath(group)
	deleted := strings.TrimSuffix(string(group), "\n") == "0::/system.slice/"+pauseWorker+" (deleted)"
	if err != nil || name != "/system.slice/"+pauseWorker {
		// A zombie may retain this deleted membership until reaped. Only the
		// already-bound original identity can use this kernel absence form.
		if !deleted || expectedStart == 0 || !reapingState(state) {
			return 0, errRefused
		}
	}
	again, err := readAt(dir, "stat", 8192)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, errPauseBusy
		}
		return 0, err
	}
	end, endState, err := statIdentity(again, int64(pid))
	if err != nil || end != start || deleted && !reapingState(endState) {
		return 0, errRefused
	}
	if !samePath(dir, path) {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return 0, errPauseBusy
		}
		return 0, errRefused
	}
	if deleted {
		return start, errPauseBusy
	}
	return start, nil
}
func (h pauseHost) group() (uint64, uint64, bool, error) {
	return serviceGroup(h.cgroup, "/system.slice/"+pauseWorker, h.realFS)
}

func serviceGroup(root, group string, realFS bool) (uint64, uint64, bool, error) {
	if (pauseHost{realFS: realFS}).fs(root, unix.CGROUP2_SUPER_MAGIC) != nil {
		return 0, 0, false, errRefused
	}
	if group == "/" || !filepath.IsAbs(group) || filepath.Clean(group) != group {
		return 0, 0, false, errRefused
	}
	path := filepath.Join(root, group)
	dir, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if os.IsNotExist(err) {
		return 0, 0, true, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	defer dir.Close()
	var st unix.Stat_t
	if unix.Fstat(int(dir.Fd()), &st) != nil {
		return 0, 0, false, errRefused
	}
	raw, err := readAt(dir, "cgroup.events", 4096)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, false, errPauseBusy
		}
		return 0, 0, false, err
	}
	found := false
	empty := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0, 0, false, errRefused
		}
		if fields[0] == "populated" {
			if found || fields[1] != "0" && fields[1] != "1" {
				return 0, 0, false, errRefused
			}
			found = true
			empty = fields[1] == "0"
		}
	}
	if !found {
		return 0, 0, false, errRefused
	}
	if !samePath(dir, path) {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return 0, 0, false, errPauseBusy
		}
		return 0, 0, false, errRefused
	}
	return st.Dev, st.Ino, empty, nil
}
func (h pauseHost) enrich(v *pauseUnits, original *pauseUnits) error {
	old := [2]pausePID{}
	if original != nil {
		old = [2]pausePID{original.Main, original.Control}
	}
	for i, p := range []*pausePID{&v.Main, &v.Control} {
		if original != nil && p.PID != 0 && p.PID != old[i].PID {
			return errRefused
		}
		start, err := h.process(p.PID, old[i].Start)
		if err != nil {
			return err
		}
		if p.PID != 0 && start == 0 {
			return errPauseBusy
		}
		p.Start = start
	}
	dev, ino, _, err := h.group()
	if err != nil {
		return err
	}
	v.GroupDevice, v.GroupInode = dev, ino
	return nil
}
func (h pauseHost) drained(ctx context.Context, before, current pauseUnits) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, p := range []pausePID{before.Main, before.Control} {
		currentStart, err := h.process(p.PID, p.Start)
		if err != nil {
			return err
		}
		if currentStart != 0 {
			if currentStart != p.Start {
				return errRefused
			}
			return errPauseBusy
		}
	}
	dev, ino, empty, err := h.group()
	if err != nil {
		return err
	}
	if dev != 0 && (dev != before.GroupDevice || ino != before.GroupInode) {
		return errRefused
	}
	if dev != current.GroupDevice || ino != current.GroupInode {
		return errPauseBusy
	}
	if !empty {
		return errPauseBusy
	}
	return nil
}
