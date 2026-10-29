package lifecycle

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type sessionSocket struct {
	Inode     uint64 `json:"inode"`
	Namespace uint64 `json:"network_namespace_inode"`
	Device    uint64 `json:"network_namespace_device"`
}

func (s sessionSocket) valid() bool { return s.Inode != 0 && s.Namespace != 0 && s.Device != 0 }

// The column contract is documented by Linux's networking/proc_net_tcp.rst.
// Only LISTEN rows for the dedicated API port are admission-bearing. TIME_WAIT
// rows may omit unrelated columns and cannot stand in for a listener.
func sessionListeningSocket(raw4, raw6 []byte, port uint16, uid uint32) (uint64, error) {
	var inode uint64
	loopback := fmt.Sprintf("%08X", binary.NativeEndian.Uint32([]byte{127, 0, 0, 1}))
	for family, raw := range [][]byte{raw4, raw6} {
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		header := strings.Fields(lines[0])
		hasInode := false
		for _, field := range header {
			hasInode = hasInode || field == "inode"
		}
		if len(header) < 4 || header[0] != "sl" || header[1] != "local_address" || header[3] != "st" || !hasInode {
			return 0, errRefused
		}
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				return 0, errRefused
			}
			address := strings.Split(fields[1], ":")
			if len(address) != 2 {
				return 0, errRefused
			}
			p, err := strconv.ParseUint(address[1], 16, 16)
			if err != nil {
				return 0, errRefused
			}
			if p != uint64(port) || fields[3] != "0A" {
				continue
			}
			if family != 0 || address[0] != loopback || len(fields) < 10 || inode != 0 {
				return 0, errRefused
			}
			owner, err := strconv.ParseUint(fields[7], 10, 32)
			if err != nil || owner != uint64(uid) {
				return 0, errRefused
			}
			inode, err = strconv.ParseUint(fields[9], 10, 64)
			if err != nil || inode == 0 {
				return 0, errRefused
			}
		}
	}
	if inode == 0 {
		return 0, errRefused
	}
	return inode, nil
}

func sessionNamespace(proc *os.File, pid uint32, realFS bool) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	// This fixed procfs namespace link is followed deliberately. It is not a
	// caller-supplied pathname, and the namespace descriptor remains pinned.
	fd, err := unix.Openat(int(proc.Fd()), strconv.FormatUint(uint64(pid), 10)+"/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, st, errRefused
	}
	f := os.NewFile(uintptr(fd), "network-namespace")
	var fs unix.Statfs_t
	if unix.Fstat(fd, &st) != nil || unix.Fstatfs(fd, &fs) != nil || realFS && fs.Type != unix.NSFS_MAGIC {
		f.Close()
		return nil, st, errRefused
	}
	return f, st, nil
}

func (h gatewayHost) listener(ctx context.Context, process serviceProcess, port uint16) (sessionSocket, error) {
	out := sessionSocket{}
	if ctx.Err() != nil || process.PID == 0 || process.Start == 0 || port == 0 || (pauseHost{realFS: h.realFS}).fs(h.proc, unix.PROC_SUPER_MAGIC) != nil {
		return out, errRefused
	}
	proc, err := openPath(h.proc, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return out, errRefused
	}
	defer proc.Close()
	self, own, err := sessionNamespace(proc, uint32(os.Getpid()), h.realFS)
	if err != nil {
		return out, errRefused
	}
	defer self.Close()
	server, target, err := sessionNamespace(proc, process.PID, h.realFS)
	if err != nil {
		return out, errRefused
	}
	defer server.Close()
	if own.Dev != target.Dev || own.Ino != target.Ino {
		return out, errRefused
	}
	dir, err := openPath(h.proc+"/"+strconv.FormatUint(uint64(process.PID), 10), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return out, errRefused
	}
	defer dir.Close()
	read := func() (uint64, error) {
		four, err := readAt(dir, "net/tcp", 8*1024*1024)
		if err != nil {
			return 0, errRefused
		}
		six, err := readAt(dir, "net/tcp6", 8*1024*1024)
		if err != nil {
			return 0, errRefused
		}
		return sessionListeningSocket(four, six, port, process.UID)
	}
	inode, err := read()
	if err != nil {
		return out, errRefused
	}
	fd, err := unix.Openat(int(dir.Fd()), "fd", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return out, errRefused
	}
	fds := os.NewFile(uintptr(fd), "service-descriptors")
	defer fds.Close()
	names, err := fds.Readdirnames(65537)
	if err != nil && err != io.EOF || len(names) > 65536 {
		return out, errRefused
	}
	found := ""
	wanted := fmt.Sprintf("socket:[%d]", inode)
	for _, name := range names {
		if ctx.Err() != nil {
			return out, errRefused
		}
		if _, err := strconv.ParseUint(name, 10, 32); err != nil {
			return out, errRefused
		}
		link := make([]byte, 256)
		n, err := unix.Readlinkat(fd, name, link)
		if err == unix.ENOENT {
			continue
		} // A concurrently closed unrelated FD is not evidence.
		if err != nil {
			return out, errRefused
		}
		if string(link[:n]) == wanted {
			found = name
			break
		}
	}
	if found == "" {
		return out, errRefused
	}
	// Re-read this same descriptor after the network inventory. A different
	// process's socket or a recycled descriptor cannot supply this binding.
	again, err := read()
	link := make([]byte, 256)
	n, linkErr := unix.Readlinkat(fd, found, link)
	if err != nil || again != inode || linkErr != nil || string(link[:n]) != wanted {
		return out, errRefused
	}
	stat, err := readAt(dir, "stat", 8192)
	start, _, statErr := statIdentity(stat, int64(process.PID))
	if err != nil || statErr != nil || start != process.Start || ctx.Err() != nil || !samePath(dir, h.proc+"/"+strconv.FormatUint(uint64(process.PID), 10)) {
		return out, errRefused
	}
	// Detect a namespace switch as well as PID reuse while reading descriptors.
	current, ns, err := sessionNamespace(proc, process.PID, h.realFS)
	if err != nil {
		return out, errRefused
	}
	current.Close()
	if ns.Dev != target.Dev || ns.Ino != target.Ino {
		return out, errRefused
	}
	return sessionSocket{inode, target.Ino, uint64(target.Dev)}, nil
}
