package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func executionFixture(t *testing.T) startup {
	t.Helper()
	s := fixture(t)
	p := readFixturePolicy(t, s)
	write(t, s.policy, p, 0644)
	active := strings.Replace(activeState, `"ready":false`, `"ready":true,"agent_id":9007199254740999`, 1)
	write(t, filepath.Join(s.state, "state.json"), []byte(strings.Replace(clearState, `"active":null`, `"active":`+active, 1)), 0600)
	return s
}

func readFixturePolicy(t *testing.T, s startup) []byte {
	t.Helper()
	return []byte(`{"controller_id":"bbbbbbbb-0000-4000-8000-000000000002","repository_id":"cccccccc-0000-4000-8000-000000000003","scaleset_name":"build","pool_id":"build-cccccccc-0000-4000-8000-000000000003","image_fingerprint":"` + strings.Repeat("1", 64) + `","provider_sha256":"` + fileSHA(t, s.native) + `","config_sha256":"` + fileSHA(t, s.config) + `"}`)
}

func TestExecutionInspectionHoldsPermanentLockAndPreservesBytes(t *testing.T) {
	s := executionFixture(t)
	before := fileSHA(t, filepath.Join(s.state, "state.json"))
	called := false
	err := s.inspectExecution(context.Background(), func(b ExecutionBinding, recheck func() error) (func() error, error) {
		called = true
		if b.Name != "runner-one" || b.AgentID != 9007199254740999 || b.Pool != "build-cccccccc-0000-4000-8000-000000000003" {
			t.Fatal("incorrect binding")
		}
		f, err := os.Open(filepath.Join(s.state, "operation.lock"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
			t.Fatalf("permanent lock not held: %v", err)
		}
		return func() error {
			f, err := os.Open(filepath.Join(s.state, "operation.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
				t.Fatal("publication released permanent lock")
			}
			return recheck()
		}, nil
	})
	if err != nil || !called {
		t.Fatal(err)
	}
	if fileSHA(t, filepath.Join(s.state, "state.json")) != before {
		t.Fatal("inspection changed state")
	}
}

func TestExecutionInspectionRefusesChangesBeforeCommit(t *testing.T) {
	for _, kind := range []string{"state bytes", "policy bytes", "native bytes", "config bytes", "maintenance", "pending", "new", "lock replacement", "directory mode"} {
		t.Run(kind, func(t *testing.T) {
			s := executionFixture(t)
			published := false
			err := s.inspectExecution(context.Background(), func(_ ExecutionBinding, recheck func() error) (func() error, error) {
				switch kind {
				case "state bytes":
					path := filepath.Join(s.state, "state.json")
					raw, _ := os.ReadFile(path)
					write(t, path, append(raw, '\n'), 0600)
				case "policy bytes":
					raw, _ := os.ReadFile(s.policy)
					write(t, s.policy, append(raw, '\n'), 0644)
				case "native bytes":
					write(t, s.native, []byte("changed"), 0755)
				case "config bytes":
					write(t, s.config, []byte("changed"), 0644)
				case "maintenance":
					write(t, s.maintenance, nil, 0600)
				case "pending":
					write(t, filepath.Join(s.state, "state.pending"), nil, 0600)
				case "new":
					write(t, filepath.Join(s.state, "state.new"), nil, 0600)
				case "lock replacement":
					path := filepath.Join(s.state, "operation.lock")
					if err := os.Rename(path, path+"-old"); err != nil {
						t.Fatal(err)
					}
					write(t, path, nil, 0600)
				case "directory mode":
					if err := os.Chmod(s.state, 0755); err != nil {
						t.Fatal(err)
					}
				}
				return func() error { published = true; return nil }, nil
			})
			if err == nil || published {
				t.Fatal("changed guard admitted")
			}
		})
	}
	for _, raw := range []string{clearState, strings.Replace(clearState, `"active":null`, `"active":`+activeState, 1)} {
		s := executionFixture(t)
		write(t, filepath.Join(s.state, "state.json"), []byte(raw), 0600)
		if err := s.inspectExecution(context.Background(), func(ExecutionBinding, func() error) (func() error, error) {
			t.Fatal("unready/vacant callback invoked")
			return nil, errRefused
		}); err == nil {
			t.Fatal("unready/vacant accepted")
		}
	}
}

func TestExecutionPublicationDefinesFinalAcceptance(t *testing.T) {
	s := executionFixture(t)
	published := false
	err := s.inspectExecution(context.Background(), func(_ ExecutionBinding, recheck func() error) (func() error, error) {
		return func() error {
			if err := recheck(); err != nil {
				return err
			}
			published = true
			// Model an observation that changes after publication. The published
			// identity describes its original instant; it is not current closure.
			write(t, s.maintenance, nil, 0600)
			return nil
		}, nil
	})
	if err != nil || !published {
		t.Fatal("post-publication observation revoked accepted identity")
	}
}
