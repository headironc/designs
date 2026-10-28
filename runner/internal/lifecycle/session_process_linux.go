package lifecycle

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type sessionProcesses struct {
	proc       string
	realFS     bool
	serviceUID uint32
	// Only synthetic fixtures synchronize a change during one observation.
	afterRead func()
}

// PF_KTHREAD is the kernel-owned task flag from include/linux/sched.h.
const sessionKernelThread = 0x00200000

func sessionProcessUIDs(raw []byte) ([4]uint32, error) {
	var ids [4]uint32
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Uid:" {
			continue
		}
		if found || len(fields) != 5 {
			return ids, errRefused
		}
		found = true
		for i, field := range fields[1:] {
			n, err := strconv.ParseUint(field, 10, 32)
			if err != nil {
				return ids, errRefused
			}
			ids[i] = uint32(n)
		}
	}
	if !found {
		return ids, errRefused
	}
	return ids, nil
}

func sessionIssuerImage(executable string) bool {
	name := filepath.Base(strings.TrimSuffix(executable, " (deleted)"))
	for _, prefix := range []string{"python", "pypy", "qemu-system-", "qemu-kvm"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "fish", "node", "nodejs", "bun", "deno", "ruby", "perl", "php", "lua", "luajit", "java", "dotnet", "curl", "wget", "gh", "garm", "garm-cli", "runner-admin", "provider-watchdog", "guarded-provider", "guarded-lxd-provider", "garm-provider-lxd":
		return true
	}
	return false
}

func sessionIssuerArgument(arg string) bool {
	for _, path := range []string{"/home/headiron/runner-deployment/halo-20261026/", "/var/lib/nucleus-halo-first-connection-inputs", "/var/lib/nucleus-halo-rebind-code", "/etc/nucleus-halo/admin-password", "/opt/nucleus-halo/bin/provider-watchdog", "/opt/nucleus-halo/bin/provider-watchdog.py"} {
		if strings.Contains(arg, path) {
			return true
		}
	}
	return strings.Contains(arg, "127.0.0.1:9997") || strings.Contains(arg, "/api/v1/auth/login")
}

func sessionProcNames(dir *os.File) ([]string, error) {
	if _, err := dir.Seek(0, io.SeekStart); err != nil {
		return nil, errRefused
	}
	names, err := dir.Readdirnames(65537)
	if err != nil && err != io.EOF || len(names) > 65536 {
		return nil, errRefused
	}
	pids := []string{}
	for _, name := range names {
		n, err := strconv.ParseUint(name, 10, 31)
		if err != nil {
			if name != "" && name[0] >= '0' && name[0] <= '9' {
				return nil, errRefused
			}
			continue
		}
		if n == 0 || strconv.FormatUint(n, 10) != name {
			return nil, errRefused
		}
		pids = append(pids, name)
	}
	sort.Strings(pids)
	return pids, nil
}

// This observes current known issuers and orphan VM processes. Original locks
// prevent the cooperating historical producers from starting another request;
// this inventory does not reconstruct the outcome of a process that has ended.
func (h sessionProcesses) check(ctx context.Context, boot string, controller serviceProcess) error {
	if h.serviceUID == 0 || !uuidPattern.MatchString(boot) || (pauseHost{proc: h.proc, realFS: h.realFS}).fs(h.proc, unix.PROC_SUPER_MAGIC) != nil {
		return errRefused
	}
	currentBoot := func() error {
		id, err := (pauseHost{proc: h.proc, realFS: h.realFS}).boot()
		if ctx.Err() != nil || err != nil || id != boot {
			return errRefused
		}
		return nil
	}
	if currentBoot() != nil {
		return errRefused
	}
	dir, err := openPath(h.proc, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return errRefused
	}
	defer dir.Close()
	if h.realFS {
		link := make([]byte, 32)
		n, err := unix.Readlinkat(int(dir.Fd()), "self", link)
		if err != nil || n >= len(link) || string(link[:n]) != strconv.Itoa(os.Getpid()) {
			return errRefused
		}
	}
	names, err := sessionProcNames(dir)
	if err != nil || len(names) == 0 {
		return errRefused
	}
	for _, name := range names {
		if ctx.Err() != nil || h.process(name, controller) != nil {
			return errRefused
		}
	}
	again, err := sessionProcNames(dir)
	if err != nil || len(again) != len(names) || !samePath(dir, h.proc) || currentBoot() != nil {
		return errRefused
	}
	for i, name := range names {
		if again[i] != name {
			return errRefused
		}
		if ctx.Err() != nil || h.process(name, controller) != nil {
			return errRefused
		}
	}
	final, err := sessionProcNames(dir)
	if err != nil || len(final) != len(names) || currentBoot() != nil {
		return errRefused
	}
	for i := range names {
		if names[i] != final[i] {
			return errRefused
		}
	}
	return nil
}

func (h sessionProcesses) process(name string, controller serviceProcess) error {
	pid, err := strconv.ParseUint(name, 10, 31)
	if err != nil || pid == 0 {
		return errRefused
	}
	path := filepath.Join(h.proc, name)
	dir, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return errRefused
	}
	defer dir.Close()
	stat, err := readAt(dir, "stat", 8192)
	start, _, statErr := statIdentity(stat, int64(pid))
	if err != nil || statErr != nil {
		return errRefused
	}
	status, err := readAt(dir, "status", 64*1024)
	ids, idsErr := sessionProcessUIDs(status)
	if err != nil || idsErr != nil {
		return errRefused
	}
	command, err := readAt(dir, "cmdline", 128*1024)
	defer clear(command)
	if err != nil {
		return errRefused
	}
	link := make([]byte, 4097)
	n, linkErr := unix.Readlinkat(int(dir.Fd()), "exe", link)
	if n >= 4096 || linkErr != nil || len(command) == 0 || command[len(command)-1] != 0 {
		// Empty command lines are accepted only for a positively identified
		// kernel thread. A zombie or unreadable userspace image remains busy.
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
		flags, e := strconv.ParseUint(fields[6], 10, 64)
		if e != nil || flags&sessionKernelThread == 0 || ids != [4]uint32{} || len(command) != 0 || !os.IsNotExist(linkErr) {
			return errRefused
		}
	} else {
		executable := string(link[:n])
		privileged := false
		for _, id := range ids {
			privileged = privileged || id == 0 || id == h.serviceUID
		}
		self := pid == uint64(os.Getpid())
		currentController := pid == uint64(controller.PID) && start == controller.Start && ids == [4]uint32{h.serviceUID, h.serviceUID, h.serviceUID, h.serviceUID} && executable == "/opt/nucleus-halo/bin/garm"
		if !self && !currentController {
			if strings.HasPrefix(filepath.Base(executable), "qemu-") || privileged && sessionIssuerImage(executable) {
				return errRefused
			}
			args := strings.Split(string(command[:len(command)-1]), "\x00")
			if args[0] == "" || privileged && sessionIssuerImage(args[0]) {
				return errRefused
			}
			for _, arg := range args {
				if sessionIssuerArgument(arg) {
					return errRefused
				}
			}
		}
		if !h.realFS && h.afterRead != nil {
			h.afterRead()
		}
		again := make([]byte, 4097)
		count, e := unix.Readlinkat(int(dir.Fd()), "exe", again)
		commandAgain, commandErr := readAt(dir, "cmdline", 128*1024)
		defer clear(commandAgain)
		if e != nil || count != n || !bytes.Equal(again[:count], link[:n]) || commandErr != nil || !bytes.Equal(commandAgain, command) {
			return errRefused
		}
	}
	statusAgain, err := readAt(dir, "status", 64*1024)
	idsAgain, e := sessionProcessUIDs(statusAgain)
	statAgain, statErr := readAt(dir, "stat", 8192)
	end, _, identityErr := statIdentity(statAgain, int64(pid))
	if err != nil || e != nil || idsAgain != ids || statErr != nil || identityErr != nil || end != start || !samePath(dir, path) {
		return errRefused
	}
	if len(command) == 0 {
		fields := strings.Fields(string(statAgain[bytes.LastIndexByte(statAgain, ')')+1:]))
		flags, e := strconv.ParseUint(fields[6], 10, 64)
		_, imageErr := unix.Readlinkat(int(dir.Fd()), "exe", link)
		cmd, commandErr := readAt(dir, "cmdline", 128*1024)
		defer clear(cmd)
		if e != nil || flags&sessionKernelThread == 0 || !os.IsNotExist(imageErr) || commandErr != nil || len(cmd) != 0 {
			return errRefused
		}
	}
	return nil
}
