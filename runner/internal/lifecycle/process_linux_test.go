package lifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func processFixture(t *testing.T) (hostFiles, string) {
	t.Helper()
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	group := filepath.Join(root, "cgroup")
	qemu := filepath.Join(root, "qemu-system-x86_64")
	testWrite(t, qemu, []byte("fixture executable"), 0755)
	testWrite(t, filepath.Join(proc, "sys/kernel/random/boot_id"), []byte(bootUUID+"\n"), 0444)
	fields := []string{"S", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "876543"}
	testWrite(t, filepath.Join(proc, "4321/stat"), []byte("4321 (qemu (test)) "+strings.Join(fields, " ")+"\n"), 0444)
	testWrite(t, filepath.Join(proc, "4321/cmdline"), []byte(qemu+"\x00-uuid\x00"+vmUUID+"\x00"), 0444)
	testWrite(t, filepath.Join(proc, "4321/cgroup"), []byte("0::/lxd/runner-one\n"), 0444)
	testWrite(t, filepath.Join(group, "lxd/runner-one/cgroup.procs"), []byte("4321\n"), 0444)
	if err := os.Symlink(qemu, filepath.Join(proc, "4321/exe")); err != nil {
		t.Fatal(err)
	}
	return hostFiles{proc: proc, cgroup: group, fixtureExecutable: qemu}, qemu
}

func TestHostIdentityAndField22(t *testing.T) {
	h, qemu := processFixture(t)
	p, err := h.observe(4321, vmUUID)
	if err != nil {
		t.Fatal(err)
	}
	if p.StartTime != 876543 || p.BootID != bootUUID || p.Executable != qemu || p.Cgroup != "/lxd/runner-one" || p.ExecutableInode == 0 || p.CgroupInode == 0 {
		t.Fatalf("wrong host identity %+v", p)
	}
	h.realFS = true
	if _, err := h.observe(4321, vmUUID); err == nil {
		t.Fatal("ordinary fixture accepted as real procfs")
	}
}

func TestHostRejectsExitedReusedAndUnboundProcess(t *testing.T) {
	for _, kind := range []string{"zombie", "overflow starttime", "wrong uuid", "duplicate uuid", "root group", "relative group", "traversal group", "double slash", "multiple groups", "symlink group", "symlink proc", "nonmember", "wrong executable", "writable executable", "missing boot", "wrong pid", "empty cmdline"} {
		t.Run(kind, func(t *testing.T) {
			h, qemu := processFixture(t)
			switch kind {
			case "zombie", "overflow starttime", "wrong pid":
				path := filepath.Join(h.proc, "4321/stat")
				raw, _ := os.ReadFile(path)
				switch kind {
				case "zombie":
					raw = []byte(strings.Replace(string(raw), ") S ", ") Z ", 1))
				case "overflow starttime":
					raw = []byte(strings.Replace(string(raw), "876543", "18446744073709551616", 1))
				case "wrong pid":
					raw = []byte(strings.Replace(string(raw), "4321 (", "4322 (", 1))
				}
				testWrite(t, path, raw, 0444)
			case "wrong uuid":
				testWrite(t, filepath.Join(h.proc, "4321/cmdline"), []byte(qemu+"\x00-uuid\x00"+volumeUUID+"\x00"), 0444)
			case "duplicate uuid":
				testWrite(t, filepath.Join(h.proc, "4321/cmdline"), []byte(qemu+"\x00-uuid\x00"+vmUUID+"\x00-uuid\x00"+vmUUID+"\x00"), 0444)
			case "empty cmdline":
				testWrite(t, filepath.Join(h.proc, "4321/cmdline"), nil, 0444)
			case "root group", "relative group", "traversal group", "double slash", "multiple groups":
				value := map[string]string{"root group": "0::/\n", "relative group": "0::relative\n", "traversal group": "0::/lxd/../runner\n", "double slash": "0::/lxd//runner\n", "multiple groups": "0::/lxd/runner-one\n0::/other\n"}[kind]
				testWrite(t, filepath.Join(h.proc, "4321/cgroup"), []byte(value), 0444)
			case "symlink group":
				original := filepath.Join(h.cgroup, "lxd/runner-one")
				if err := os.Rename(original, original+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original+"-real", original); err != nil {
					t.Fatal(err)
				}
			case "symlink proc":
				original := filepath.Join(h.proc, "4321")
				if err := os.Rename(original, original+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original+"-real", original); err != nil {
					t.Fatal(err)
				}
			case "nonmember":
				testWrite(t, filepath.Join(h.cgroup, "lxd/runner-one/cgroup.procs"), []byte("4322\n"), 0444)
			case "wrong executable":
				h.fixtureExecutable = qemu + "-other"
			case "writable executable":
				if err := os.Chmod(qemu, 0777); err != nil {
					t.Fatal(err)
				}
			case "missing boot":
				if err := os.Remove(filepath.Join(h.proc, "sys/kernel/random/boot_id")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.observe(4321, vmUUID); err == nil {
				t.Fatal("accepted invalid process")
			}
		})
	}
}

func TestActualProcessHelper(t *testing.T) {
	if os.Getenv("RUNNER_CAPTURE_PROCESS_FIXTURE") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	var value [1]byte
	os.Stdin.Read(value[:])
	os.Exit(0)
}

func TestActualProcessIdentityAndExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rawExecutable, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	executable = filepath.Join(t.TempDir(), "qemu-fixture")
	testWrite(t, executable, rawExecutable, 0755)
	cmd := exec.Command(executable, "-test.run=^TestActualProcessHelper$", "--", "-uuid", vmUUID)
	cmd.Env = append(os.Environ(), "RUNNER_CAPTURE_PROCESS_FIXTURE=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	ready := make(chan error, 1)
	go func() {
		var raw [6]byte
		_, err := stdout.Read(raw[:])
		if err == nil && string(raw[:]) != "ready\n" {
			err = errRefused
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess did not start")
	}
	pid := int64(cmd.Process.Pid)
	raw, err := os.ReadFile("/proc/" + strconv.FormatInt(pid, 10) + "/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	group, err := groupPath(raw)
	if err != nil {
		t.Skip("test process has no non-root unified cgroup; synthetic fixtures cover group rejection")
	}
	root := t.TempDir()
	testWrite(t, filepath.Join(root, group, "cgroup.procs"), []byte(strconv.FormatInt(pid, 10)+"\n"), 0444)
	h := hostFiles{proc: "/proc", cgroup: root, fixtureExecutable: executable}
	first, err := h.observe(pid, vmUUID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.observe(pid, vmUUID)
	if err != nil || first != second {
		t.Fatal("real live process identity changed")
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.observe(pid, vmUUID); err == nil {
		t.Fatal("exited process accepted")
	}
}
