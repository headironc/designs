package lifecycle

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type gatewayHost struct {
	proc, cgroup         string
	realFS               bool
	nginx, controllerExe string
}

func processIDs(raw []byte) (uint32, uint32, error) {
	values := map[string]uint32{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Uid:" && fields[0] != "Gid:" {
			continue
		}
		if _, seen := values[fields[0]]; seen || len(fields) != 5 {
			return 0, 0, errRefused
		}
		var selected uint64
		for i, v := range fields[1:] {
			id, err := strconv.ParseUint(v, 10, 32)
			if err != nil || id == 0 || i > 0 && id != selected {
				return 0, 0, errRefused
			}
			selected = id
		}
		values[fields[0]] = uint32(selected)
	}
	if len(values) != 2 {
		return 0, 0, errRefused
	}
	return values["Uid:"], values["Gid:"], nil
}

func (h gatewayHost) process(pid uint32, unit string, original *serviceProcess) (serviceProcess, error) {
	out := serviceProcess{PID: pid}
	if pid == 0 {
		return serviceProcess{}, nil
	}
	if pid > 2147483647 || (pauseHost{realFS: h.realFS}).fs(h.proc, unix.PROC_SUPER_MAGIC) != nil {
		return out, errRefused
	}
	executable := h.nginx
	if unit == controllerUnit {
		executable = h.controllerExe
	} else if unit != gatewayUnit {
		return out, errRefused
	}
	path := filepath.Join(h.proc, strconv.FormatUint(uint64(pid), 10))
	dir, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if os.IsNotExist(err) {
		return serviceProcess{}, nil
	}
	if err != nil {
		return out, err
	}
	defer dir.Close()
	stat, err := readAt(dir, "stat", 8192)
	if os.IsNotExist(err) {
		return out, errPauseBusy
	}
	if err != nil {
		return out, err
	}
	start, state, err := statIdentity(stat, int64(pid))
	if err != nil || original != nil && (original.PID != pid || original.Start != start) {
		return out, errRefused
	}
	out.Start = start
	status, err := readAt(dir, "status", 64*1024)
	if os.IsNotExist(err) {
		return out, errPauseBusy
	}
	if err != nil {
		return out, err
	}
	out.UID, out.GID, err = processIDs(status)
	if err != nil || original != nil && (out.UID != original.UID || out.GID != original.GID) {
		return out, errRefused
	}
	var procStat unix.Stat_t
	if unix.Fstat(int(dir.Fd()), &procStat) != nil || h.realFS && procStat.Uid != out.UID {
		return out, errRefused
	}
	membership, err := readAt(dir, "cgroup", 4096)
	if os.IsNotExist(err) {
		return out, errPauseBusy
	}
	if err != nil {
		return out, err
	}
	group := "0::/system.slice/" + unit
	member := strings.TrimSuffix(string(membership), "\n")
	deleted := member == group+" (deleted)"
	if member != group && !(deleted && original != nil && reapingState(state)) {
		return out, errRefused
	}
	if reapingState(state) {
		if original == nil {
			return out, errRefused
		}
		out.ExecutableDevice, out.ExecutableInode, out.ExecutableSHA = original.ExecutableDevice, original.ExecutableInode, original.ExecutableSHA
	} else {
		if deleted {
			return out, errRefused
		}
		link := make([]byte, 4096)
		n, e := unix.Readlinkat(int(dir.Fd()), "exe", link)
		if e != nil || string(link[:n]) != executable {
			return out, errRefused
		}
		fd, e := unix.Openat(int(dir.Fd()), "exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if e != nil {
			if os.IsNotExist(e) {
				return out, errPauseBusy
			}
			return out, errRefused
		}
		image := os.NewFile(uintptr(fd), "service-image")
		defer image.Close()
		var st unix.Stat_t
		e = unix.Fstat(fd, &st)
		if e != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 || st.Mode&0111 == 0 || st.Nlink != 1 || h.realFS && st.Uid != 0 || !samePath(image, executable) {
			return out, errRefused
		}
		if h.realFS && (protectedFiles{uid: 0, boundary: "/"}).parents(executable) != nil {
			return out, errRefused
		}
		sum := sha256.New()
		count, e := io.Copy(sum, io.LimitReader(image, 256*1024*1024+1))
		if e != nil || count > 256*1024*1024 {
			return out, errRefused
		}
		out.ExecutableDevice, out.ExecutableInode, out.ExecutableSHA = st.Dev, st.Ino, fmt.Sprintf("%x", sum.Sum(nil))
		check, e := unix.Openat(int(dir.Fd()), "exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if e != nil {
			if os.IsNotExist(e) {
				return out, errPauseBusy
			}
			return out, errRefused
		}
		var imageAgain unix.Stat_t
		e = unix.Fstat(check, &imageAgain)
		unix.Close(check)
		if e != nil || imageAgain.Dev != st.Dev || imageAgain.Ino != st.Ino || !samePath(image, executable) {
			return out, errRefused
		}
		if original != nil && out != *original {
			return out, errRefused
		}
	}
	again, err := readAt(dir, "stat", 8192)
	if os.IsNotExist(err) {
		return out, errPauseBusy
	}
	if err != nil {
		return out, err
	}
	end, endState, err := statIdentity(again, int64(pid))
	if err != nil || end != start || deleted && !reapingState(endState) {
		return out, errRefused
	}
	statusAgain, e := readAt(dir, "status", 64*1024)
	if os.IsNotExist(e) {
		return out, errPauseBusy
	}
	uid, gid, idsErr := processIDs(statusAgain)
	if e != nil || idsErr != nil || uid != out.UID || gid != out.GID {
		return out, errRefused
	}
	if !samePath(dir, path) {
		if _, e := os.Lstat(path); os.IsNotExist(e) {
			return out, errPauseBusy
		}
		return out, errRefused
	}
	if reapingState(state) || reapingState(endState) {
		return out, errPauseBusy
	}
	return out, nil
}

func (h gatewayHost) enrich(v *gatewayFacts, original *gatewayFacts) error {
	busy := false
	old := [2]*serviceProcess{}
	if original != nil {
		old = [2]*serviceProcess{&original.Main, &original.Control}
	}
	for i, p := range []*serviceProcess{&v.Main, &v.Control} {
		if original != nil && p.PID != 0 && p.PID != old[i].PID {
			return errRefused
		}
		fact, err := h.process(p.PID, gatewayUnit, old[i])
		if err != nil {
			if err == errPauseBusy {
				busy = true
				continue
			}
			return err
		}
		if p.PID != 0 && fact.PID == 0 {
			busy = true
			continue
		}
		*p = fact
	}
	dev, ino, _, err := serviceGroup(h.cgroup, gatewayGroup, h.realFS)
	if err != nil {
		if err == errPauseBusy {
			return errPauseBusy
		}
		return err
	}
	v.GroupDevice, v.GroupInode = dev, ino
	if original != nil && dev != 0 && (dev != original.GroupDevice || ino != original.GroupInode) {
		return errRefused
	}
	if busy {
		return errPauseBusy
	}
	return nil
}

func (h gatewayHost) drained(ctx context.Context, before, current gatewayFacts) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	busy := false
	for _, old := range []serviceProcess{before.Main, before.Control} {
		fact, err := h.process(old.PID, gatewayUnit, &old)
		if err != nil {
			if err == errPauseBusy {
				busy = true
				continue
			}
			return err
		}
		if fact.PID != 0 {
			busy = true
		}
	}
	dev, ino, empty, err := serviceGroup(h.cgroup, gatewayGroup, h.realFS)
	if err != nil {
		return err
	}
	if dev != 0 && (dev != before.GroupDevice || ino != before.GroupInode) {
		return errRefused
	}
	if dev != current.GroupDevice || ino != current.GroupInode || !empty || busy {
		return errPauseBusy
	}
	return nil
}
