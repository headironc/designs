package lifecycle

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func sessionProcStat(pid uint32, state, start, flags string) []byte {
	fields := strings.Fields("S 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 123")
	fields[0], fields[6], fields[19] = state, flags, start
	return []byte(fmt.Sprintf("%d (synthetic process) %s\n", pid, strings.Join(fields, " ")))
}

func sessionFixturePID() uint32 {
	if os.Getpid() == 9001 {
		return 9003
	}
	return 9001
}

func sessionProcFixture(t *testing.T, proc string, pid, uid uint32, executable, command string) string {
	t.Helper()
	dir := filepath.Join(proc, strconv.FormatUint(uint64(pid), 10))
	testWrite(t, filepath.Join(dir, "stat"), sessionProcStat(pid, "S", "123", "0"), 0444)
	testWrite(t, filepath.Join(dir, "status"), []byte(fmt.Sprintf("Uid:\t%d %d %d %d\n", uid, uid, uid, uid)), 0444)
	testWrite(t, filepath.Join(dir, "cmdline"), []byte(command), 0444)
	if executable != "" {
		if err := os.Symlink(executable, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSessionProcessesRejectLiveIssuersAndOrphanVMs(t *testing.T) {
	for _, tc := range []struct {
		name, executable, command string
		uid                       uint32
		allowed                   bool
	}{
		{"daemon", "/usr/lib/systemd/systemd", "systemd\x00", 0, true},
		{"sudo_waiter", "/usr/bin/sudo", "sudo\x00/opt/nucleus-halo/bin/runner-admin\x00acquire-session\x00", 0, true},
		{"renamed_interpreter", "/usr/local/bin/renamed", "python3\x00unknown.py\x00", 0, false},
		{"unprivileged_python", "/usr/bin/python3.14", "python3\x00unrelated.py\x00", 12000, true},
		{"root_python", "/usr/bin/python3.14", "python3\x00unknown.py\x00", 0, false},
		{"service_python", "/usr/bin/python3.14", "python3\x00unknown.py\x00", 24567, false},
		{"root_shell", "/usr/bin/bash", "bash\x00", 0, false},
		{"legacy_root", "/usr/local/bin/renamed", "renamed\x00/home/headiron/runner-deployment/halo-20261026/provider-watchdog.py\x00", 0, false},
		{"legacy_user", "/usr/local/bin/renamed", "renamed\x00/home/headiron/runner-deployment/halo-20261026/acquire-session.py\x00", 12000, false},
		{"root_login", "/usr/local/bin/renamed", "renamed\x00http://127.0.0.1:9997/api/v1/auth/login\x00", 0, false},
		{"watchdog", "/opt/nucleus-halo/bin/provider-watchdog", "provider-watchdog\x00", 0, false},
		{"another_admin", "/opt/nucleus-halo/bin/runner-admin", "runner-admin\x00", 0, false},
		{"qemu", "/snap/lxd/123/bin/qemu-system-x86_64", "qemu-system-x86_64\x00", 12000, false},
		{"deleted_qemu", "/snap/lxd/123/bin/qemu-system-x86_64 (deleted)", "qemu-system-x86_64\x00", 12000, false},
		{"wrong_controller", "/opt/nucleus-halo/bin/garm", "garm\x00-config\x00/etc/nucleus-halo/garm.toml\x00", 24567, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc := t.TempDir()
			testWrite(t, filepath.Join(proc, "sys/kernel/random/boot_id"), []byte(prepareFixtureBoot+"\n"), 0444)
			sessionProcFixture(t, proc, sessionFixturePID(), tc.uid, tc.executable, tc.command)
			h := sessionProcesses{proc: proc, serviceUID: 24567}
			if err := h.check(context.Background(), prepareFixtureBoot, serviceProcess{}); (err == nil) != tc.allowed {
				t.Fatal("issuer process admission", err)
			}
		})
	}
}

func TestSessionProcessIdentityRaces(t *testing.T) {
	for _, mode := range []string{"original", "pid_reuse", "uid_change", "exe_change", "command_change", "new_pid", "missing_status", "empty_command", "zombie", "kernel_thread", "wrong_boot", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			proc := t.TempDir()
			testWrite(t, filepath.Join(proc, "sys/kernel/random/boot_id"), []byte(prepareFixtureBoot+"\n"), 0444)
			pid := sessionFixturePID()
			dir := sessionProcFixture(t, proc, pid, 0, "/usr/lib/systemd/systemd", "systemd\x00")
			h := sessionProcesses{proc: proc, serviceUID: 24567}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "missing_status":
				if err := os.Remove(filepath.Join(dir, "status")); err != nil {
					t.Fatal(err)
				}
			case "empty_command", "zombie", "kernel_thread":
				testWrite(t, filepath.Join(dir, "cmdline"), nil, 0444)
				if err := os.Remove(filepath.Join(dir, "exe")); err != nil {
					t.Fatal(err)
				}
				if mode == "zombie" {
					testWrite(t, filepath.Join(dir, "stat"), sessionProcStat(pid, "Z", "123", "0"), 0444)
				}
				if mode == "kernel_thread" {
					testWrite(t, filepath.Join(dir, "stat"), sessionProcStat(pid, "S", "123", "2097152"), 0444)
				}
			case "wrong_boot":
				testWrite(t, filepath.Join(proc, "sys/kernel/random/boot_id"), []byte("11111111-1111-4111-8111-111111111111\n"), 0444)
			case "cancel":
				cancel()
			default:
				changed := false
				h.afterRead = func() {
					if changed {
						return
					}
					changed = true
					switch mode {
					case "pid_reuse":
						testWrite(t, filepath.Join(dir, "stat"), sessionProcStat(pid, "S", "124", "0"), 0444)
					case "uid_change":
						testWrite(t, filepath.Join(dir, "status"), []byte("Uid: 24567 24567 24567 24567\n"), 0444)
					case "exe_change":
						if err := os.Remove(filepath.Join(dir, "exe")); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink("/usr/bin/python3", filepath.Join(dir, "exe")); err != nil {
							t.Fatal(err)
						}
					case "command_change":
						testWrite(t, filepath.Join(dir, "cmdline"), []byte("python3\x00unknown.py\x00"), 0444)
					case "new_pid":
						sessionProcFixture(t, proc, 9002, 0, "/usr/bin/python3", "python3\x00unknown.py\x00")
					}
				}
			}
			if err := h.check(ctx, prepareFixtureBoot, serviceProcess{}); (err == nil) != (mode == "original" || mode == "kernel_thread") {
				t.Fatal("process identity admission", err)
			}
		})
	}
}

func TestSessionProcessesBindControllerAndCurrentAdmin(t *testing.T) {
	proc := t.TempDir()
	testWrite(t, filepath.Join(proc, "sys/kernel/random/boot_id"), []byte(prepareFixtureBoot+"\n"), 0444)
	sessionProcFixture(t, proc, uint32(os.Getpid()), 0, "/opt/nucleus-halo/bin/runner-admin", "runner-admin\x00")
	pid := sessionFixturePID()
	sessionProcFixture(t, proc, pid, 24567, "/opt/nucleus-halo/bin/garm", "garm\x00-config\x00/etc/nucleus-halo/garm.toml\x00")
	h := sessionProcesses{proc: proc, serviceUID: 24567}
	controller := serviceProcess{PID: pid, Start: 123}
	if err := h.check(context.Background(), prepareFixtureBoot, controller); err != nil {
		t.Fatal("current controller/admin were not bound", err)
	}
	controller.Start++
	if h.check(context.Background(), prepareFixtureBoot, controller) == nil {
		t.Fatal("reused controller identity admitted")
	}
}
