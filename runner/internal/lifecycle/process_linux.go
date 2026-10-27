package lifecycle

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type process struct {
	BootID           string `json:"boot_id"`
	PID              int64  `json:"pid"`
	StartTime        uint64 `json:"starttime_ticks"`
	Executable       string `json:"executable"`
	ExecutableDevice uint64 `json:"executable_device"`
	ExecutableInode  uint64 `json:"executable_inode"`
	Cgroup           string `json:"cgroup_v2"`
	CgroupDevice     uint64 `json:"cgroup_device"`
	CgroupInode      uint64 `json:"cgroup_inode"`
}

type hostFiles struct {
	proc, cgroup      string
	realFS            bool
	fixtureExecutable string
}

var qemuPath = regexp.MustCompile(`^/snap/lxd/[0-9]+/bin/qemu-system-x86_64$`)

func readAt(dir *os.File, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errRefused
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errRefused
	}
	return raw, nil
}

func startTime(raw []byte, pid int64) (uint64, error) {
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 || !bytes.HasPrefix(raw, []byte(strconv.FormatInt(pid, 10)+" (")) {
		return 0, errRefused
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" || fields[0] == "x" {
		return 0, errRefused
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, errRefused
	}
	return start, nil
}

func groupPath(raw []byte) (string, error) {
	s := strings.TrimSuffix(string(raw), "\n")
	if !strings.HasPrefix(s, "0::/") || strings.ContainsRune(s, '\n') {
		return "", errRefused
	}
	p := strings.TrimPrefix(s, "0::")
	if p == "/" || filepath.Clean(p) != p || strings.ContainsAny(p, "\x00\r\t ") || !filepath.IsAbs(p) {
		return "", errRefused
	}
	return p, nil
}

func (h hostFiles) observe(pid int64, uuid string) (process, error) {
	if pid <= 0 || pid > 2147483647 || !uuidPattern.MatchString(uuid) {
		return process{}, errRefused
	}
	proc, err := openPath(h.proc, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return process{}, err
	}
	defer proc.Close()
	if h.realFS {
		var fs unix.Statfs_t
		if unix.Fstatfs(int(proc.Fd()), &fs) != nil || fs.Type != unix.PROC_SUPER_MAGIC {
			return process{}, errRefused
		}
	}
	bootPath := filepath.Join(h.proc, "sys/kernel/random/boot_id")
	bootFile, err := openPath(bootPath, unix.O_RDONLY, 0)
	if err != nil {
		return process{}, err
	}
	defer bootFile.Close()
	boot, err := io.ReadAll(io.LimitReader(bootFile, 128))
	bootID := strings.TrimSpace(string(boot))
	if err != nil || !uuidPattern.MatchString(bootID) {
		return process{}, errRefused
	}
	pidPath := filepath.Join(h.proc, strconv.FormatInt(pid, 10))
	dir, err := openPath(pidPath, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return process{}, err
	}
	defer dir.Close()
	stat, err := readAt(dir, "stat", 8192)
	if err != nil {
		return process{}, err
	}
	start, err := startTime(stat, pid)
	if err != nil {
		return process{}, err
	}
	cmd, err := readAt(dir, "cmdline", 128*1024)
	if err != nil || len(cmd) == 0 || cmd[len(cmd)-1] != 0 {
		return process{}, errRefused
	}
	args := strings.Split(string(cmd[:len(cmd)-1]), "\x00")
	count := 0
	for i, arg := range args {
		if arg == "-uuid" {
			if i+1 >= len(args) || args[i+1] != uuid {
				return process{}, errRefused
			}
			count++
		}
	}
	if count != 1 {
		return process{}, errRefused
	}
	link := make([]byte, 4096)
	n, err := unix.Readlinkat(int(dir.Fd()), "exe", link)
	if err != nil || n == len(link) {
		return process{}, errRefused
	}
	executable := string(link[:n])
	if h.fixtureExecutable != "" {
		if executable != h.fixtureExecutable {
			return process{}, errRefused
		}
	} else if !qemuPath.MatchString(executable) {
		return process{}, errRefused
	}
	// exe is the one intentional procfs magic link. Compare its actual open
	// object with the protected canonical executable, never follow guest paths.
	fd, err := unix.Openat(int(dir.Fd()), "exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return process{}, err
	}
	exe := os.NewFile(uintptr(fd), "exe")
	defer exe.Close()
	var executableStat unix.Stat_t
	if unix.Fstat(fd, &executableStat) != nil || executableStat.Mode&unix.S_IFMT != unix.S_IFREG || executableStat.Mode&0022 != 0 || executableStat.Mode&0111 == 0 || h.realFS && executableStat.Uid != 0 || !samePath(exe, executable) {
		return process{}, errRefused
	}
	cgroupRaw, err := readAt(dir, "cgroup", 4096)
	if err != nil {
		return process{}, err
	}
	group, err := groupPath(cgroupRaw)
	if err != nil {
		return process{}, err
	}
	groupDir, err := openPath(filepath.Join(h.cgroup, group), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return process{}, err
	}
	defer groupDir.Close()
	var fs unix.Statfs_t
	var groupStat unix.Stat_t
	if unix.Fstatfs(int(groupDir.Fd()), &fs) != nil || h.realFS && fs.Type != unix.CGROUP2_SUPER_MAGIC || unix.Fstat(int(groupDir.Fd()), &groupStat) != nil {
		return process{}, errRefused
	}
	procs, err := readAt(groupDir, "cgroup.procs", 1024*1024)
	if err != nil {
		return process{}, err
	}
	found := false
	for _, p := range strings.Fields(string(procs)) {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v <= 0 {
			return process{}, errRefused
		}
		if v == pid {
			found = true
		}
	}
	if !found {
		return process{}, errRefused
	}
	// Re-read through the pinned process directory to detect exit/reuse and a
	// move during observation; later rounds compare boot, inode and starttime.
	stat, err = readAt(dir, "stat", 8192)
	if err != nil {
		return process{}, err
	}
	end, err := startTime(stat, pid)
	groupAgain, groupErr := readAt(dir, "cgroup", 4096)
	cmdAgain, cmdErr := readAt(dir, "cmdline", 128*1024)
	bootCurrent, bootErr := openPath(bootPath, unix.O_RDONLY, 0)
	if bootErr != nil {
		return process{}, bootErr
	}
	bootAgain, bootErr := io.ReadAll(io.LimitReader(bootCurrent, 128))
	bootCurrent.Close()
	currentFD, exeErr := unix.Openat(int(dir.Fd()), "exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if exeErr != nil {
		return process{}, exeErr
	}
	var currentExe unix.Stat_t
	exeErr = unix.Fstat(currentFD, &currentExe)
	unix.Close(currentFD)
	if err != nil || end != start || groupErr != nil || !bytes.Equal(groupAgain, cgroupRaw) || cmdErr != nil || !bytes.Equal(cmdAgain, cmd) || exeErr != nil || currentExe.Dev != executableStat.Dev || currentExe.Ino != executableStat.Ino || bootErr != nil || !bytes.Equal(bootAgain, boot) || !samePath(dir, pidPath) || !samePath(groupDir, filepath.Join(h.cgroup, group)) || !samePath(bootFile, bootPath) {
		return process{}, errRefused
	}
	return process{bootID, pid, start, executable, uint64(executableStat.Dev), executableStat.Ino, group, uint64(groupStat.Dev), groupStat.Ino}, nil
}

func (p process) valid() bool {
	return uuidPattern.MatchString(p.BootID) && p.PID > 0 && p.PID <= 2147483647 && p.StartTime > 0 && p.Executable != "" && p.ExecutableInode > 0 && p.CgroupInode > 0 && p.Cgroup != "/" && strings.HasPrefix(p.Cgroup, "/")
}
