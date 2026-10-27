package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const clearState = `{"version":1,"active":null,"intent":null,"latch":null}`
const activeState = `{"name":"runner-one","uuid":"11111111-2222-4333-8444-555555555555","ready":false,"at":1793000000.125}`

func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) startup {
	t.Helper()
	dir := t.TempDir()
	s := startup{
		policy: filepath.Join(dir, "policy.json"), native: filepath.Join(dir, "native"), config: filepath.Join(dir, "config.toml"),
		state: filepath.Join(dir, "state"), maintenance: filepath.Join(dir, "maintenance"),
		rootUID: uint32(os.Geteuid()), serviceUID: uint32(os.Geteuid()),
	}
	if err := os.Mkdir(s.state, 0700); err != nil {
		t.Fatal(err)
	}
	// Executing this fixture would fail; startup must only read its bytes.
	write(t, s.native, []byte("not an executable\n"), 0755)
	write(t, s.config, []byte("secret = 'never-log-this'\n"), 0644)
	policy := map[string]string{"provider_sha256": fileSHA(t, s.native), "config_sha256": fileSHA(t, s.config), "controller_id": "owned-by-other-checks"}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s.policy, raw, 0644)
	write(t, filepath.Join(s.state, "state.json"), []byte(clearState), 0600)
	write(t, filepath.Join(s.state, "operation.lock"), nil, 0600)
	return s
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestStartupPreservesStateAndArtifacts(t *testing.T) {
	for _, active := range []string{"null", activeState, strings.Replace(activeState, `"ready":false`, `"ready":true,"agent_id":9007199254740993`, 1)} {
		t.Run(active, func(t *testing.T) {
			s := fixture(t)
			write(t, filepath.Join(s.state, "state.json"), []byte(strings.Replace(clearState, `"active":null`, `"active":`+active, 1)), 0600)
			paths := []string{s.policy, s.config, s.native, filepath.Join(s.state, "state.json"), filepath.Join(s.state, "operation.lock")}
			before := make(map[string]string)
			for _, path := range paths {
				before[path] = fileSHA(t, path)
			}
			if stage, err := s.check(); err != nil {
				t.Fatalf("%s: %v", stage, err)
			}
			for _, path := range paths {
				if fileSHA(t, path) != before[path] {
					t.Fatalf("startup changed %s", path)
				}
			}
		})
	}
}

func TestStartupRefusesState(t *testing.T) {
	cases := map[string]string{
		"intent":           strings.Replace(clearState, `"intent":null`, `"intent":{"command":"CreateInstance"}`, 1),
		"latch":            strings.Replace(clearState, `"latch":null`, `"latch":{"blocks_delete":false}`, 1),
		"empty intent":     strings.Replace(clearState, `"intent":null`, `"intent":{}`, 1),
		"false latch":      strings.Replace(clearState, `"latch":null`, `"latch":false`, 1),
		"missing intent":   `{"version":1,"active":null,"latch":null}`,
		"missing active":   `{"version":1,"intent":null,"latch":null}`,
		"unknown version":  strings.Replace(clearState, `"version":1`, `"version":2`, 1),
		"null version":     strings.Replace(clearState, `"version":1`, `"version":null`, 1),
		"unknown field":    strings.Replace(clearState, `"version":1`, `"version":1,"extra":true`, 1),
		"duplicate intent": strings.Replace(clearState, `"intent":null`, `"intent":{},"intent":null`, 1),
		"case alias":       strings.Replace(clearState, `"intent":null`, `"intent":{},"Intent":null`, 1),
		"trailing value":   clearState + `{}`,
		"null":             `null`, "truncated": `{"version":1`, "large": strings.Repeat(" ", 65537),
	}
	for name, active := range map[string]string{
		"empty": `{}`, "array": `[]`, "missing ready": strings.Replace(activeState, `"ready":false,`, "", 1),
		"bad uuid":         strings.Replace(activeState, "11111111-2222-4333-8444-555555555555", "bad", 1),
		"negative time":    strings.Replace(activeState, "1793000000.125", "-1", 1),
		"missing agent":    strings.Replace(activeState, `"ready":false`, `"ready":true`, 1),
		"fractional agent": strings.Replace(activeState, `"ready":false`, `"ready":true,"agent_id":1.5`, 1),
		"duplicate ready":  strings.Replace(activeState, `"ready":false`, `"ready":true,"ready":false`, 1),
	} {
		cases["active "+name] = strings.Replace(clearState, `"active":null`, `"active":`+active, 1)
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			s := fixture(t)
			path := filepath.Join(s.state, "state.json")
			write(t, path, []byte(raw), 0600)
			if stage, err := s.check(); err == nil || stage != "state" {
				t.Fatalf("got %s, %v", stage, err)
			}
			if got, _ := os.ReadFile(path); string(got) != raw {
				t.Fatal("refusal changed original state")
			}
		})
	}
}

func TestStartupRefusesFiles(t *testing.T) {
	for _, target := range []string{"policy", "native", "config", "state", "lock"} {
		for _, kind := range []string{"missing", "symlink", "hardlink", "fifo", "directory", "mode", "special mode"} {
			t.Run(target+"/"+kind, func(t *testing.T) {
				s := fixture(t)
				path := map[string]string{"policy": s.policy, "native": s.native, "config": s.config, "state": filepath.Join(s.state, "state.json"), "lock": filepath.Join(s.state, "operation.lock")}[target]
				original := path + ".original"
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(original, path)
				case "hardlink":
					err = os.Link(original, path)
				case "fifo":
					err = unix.Mkfifo(path, 0600)
				case "directory":
					err = os.Mkdir(path, 0700)
				case "mode", "special mode":
					data, e := os.ReadFile(original)
					if e != nil {
						t.Fatal(e)
					}
					mode := os.FileMode(0666)
					if kind == "special mode" {
						info, e := os.Stat(original)
						if e != nil {
							t.Fatal(e)
						}
						mode = info.Mode().Perm() | os.ModeSetuid
					}
					write(t, path, data, mode)
				}
				if err != nil {
					t.Fatal(err)
				}
				if stage, err := s.check(); err == nil {
					t.Fatalf("admitted %s: %s", kind, stage)
				}
			})
		}
	}
}

func TestStartupRefusesBindingsAndDigests(t *testing.T) {
	cases := map[string]func(*testing.T, *startup){
		"root owner":    func(t *testing.T, s *startup) { s.rootUID++ },
		"service owner": func(t *testing.T, s *startup) { s.serviceUID++ },
		"directory mode": func(t *testing.T, s *startup) {
			if err := os.Chmod(s.state, 0755); err != nil {
				t.Fatal(err)
			}
		},
		"directory symlink": func(t *testing.T, s *startup) {
			p := s.state + ".link"
			if err := os.Symlink(s.state, p); err != nil {
				t.Fatal(err)
			}
			s.state = p
		},
		"parent symlink": func(t *testing.T, s *startup) {
			p := filepath.Join(t.TempDir(), "parent")
			if err := os.Symlink(filepath.Dir(s.policy), p); err != nil {
				t.Fatal(err)
			}
			s.policy = filepath.Join(p, "policy.json")
		},
		"native digest": func(t *testing.T, s *startup) { write(t, s.native, []byte("changed"), 0755) },
		"config digest": func(t *testing.T, s *startup) { write(t, s.config, []byte("changed"), 0644) },
		"policy digest": func(t *testing.T, s *startup) {
			write(t, s.policy, []byte(`{"provider_sha256":"bad","config_sha256":"bad"}`), 0644)
		},
		"policy duplicate": func(t *testing.T, s *startup) {
			raw, e := os.ReadFile(s.policy)
			if e != nil {
				t.Fatal(e)
			}
			write(t, s.policy, append([]byte(`{"provider_sha256":"bad",`), raw[1:]...), 0644)
		},
		"policy missing": func(t *testing.T, s *startup) { write(t, s.policy, []byte(`{}`), 0644) },
		"policy large":   func(t *testing.T, s *startup) { write(t, s.policy, bytes.Repeat([]byte(" "), 65537), 0644) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := fixture(t)
			change(t, &s)
			if _, err := s.check(); err == nil {
				t.Fatal("admitted changed input")
			}
		})
	}
}

func TestMaintenanceAlwaysRefusedAndUntouched(t *testing.T) {
	for _, kind := range []string{"permit", "directory", "dangling link", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			s := fixture(t)
			var err error
			switch kind {
			case "permit":
				write(t, s.maintenance, []byte("old permit must not be consumed"), 0440)
			case "directory":
				err = os.Mkdir(s.maintenance, 0750)
			case "dangling link":
				err = os.Symlink("missing-permit", s.maintenance)
			case "fifo":
				err = unix.Mkfifo(s.maintenance, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(s.maintenance)
			if err != nil {
				t.Fatal(err)
			}
			if stage, err := s.check(); err == nil || stage != "maintenance" {
				t.Fatalf("%s: %v", stage, err)
			}
			after, err := os.Lstat(s.maintenance)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("maintenance changed")
			}
			if kind == "permit" {
				raw, err := os.ReadFile(s.maintenance)
				if err != nil || string(raw) != "old permit must not be consumed" {
					t.Fatal("permit changed")
				}
			}
		})
	}
}

func TestOperationLockAcrossProcesses(t *testing.T) {
	s := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockProcess$")
	cmd.Env = append(os.Environ(), "GUARD_LOCK_HELPER="+filepath.Join(s.state, "operation.lock"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cmd.Wait() }()
	ready, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || ready != "locked\n" {
		t.Fatalf("helper: %q %v", ready, err)
	}
	if _, err := s.check(); !errors.Is(err, errBusy) {
		t.Fatalf("expected busy: %v", err)
	}
	in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if stage, err := s.check(); err != nil {
		t.Fatalf("lock not released: %s %v", stage, err)
	}
}

func TestLockProcess(t *testing.T) {
	path := os.Getenv("GUARD_LOCK_HELPER")
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	io.Copy(io.Discard, os.Stdin)
}

func TestPinnedFilesDetectReplacement(t *testing.T) {
	s := fixture(t)
	dir, err := openPath(s.state, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	f, err := openAt(dir, "state.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	path := filepath.Join(s.state, "state.json")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	write(t, path, []byte(clearState), 0600)
	if sameAt(f, dir, "state.json") {
		t.Fatal("accepted replacement state")
	}
	if err := os.Rename(s.state, s.state+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.state, 0700); err != nil {
		t.Fatal(err)
	}
	if samePath(dir, s.state) {
		t.Fatal("accepted replacement directory")
	}
}
