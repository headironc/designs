package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func deleteFixture(t *testing.T) (startup, map[string]string, deleter) {
	t.Helper()
	s, _, env, _, _ := createFixture(t)
	env["GARM_COMMAND"], env["GARM_INSTANCE_ID"] = "DeleteInstance", "runner-one"
	delete(env, "GARM_POOL_ID")
	write(t, filepath.Join(s.state, "state.json"), []byte(fmt.Sprintf(`{"version":1,"active":{"name":"runner-one","uuid":%q,"ready":true,"agent_id":42,"at":1793000000},"intent":null,"latch":null}`, controllerID)), 0600)
	d := deleter{
		before: func(context.Context, guardPolicy, activeInstance) (*int64, error) { id := int64(42); return &id, nil },
		after:  func(context.Context) error { return nil }, now: time.Now,
	}
	d.invoke = func(_ context.Context, g *guardFiles, _ deletePolicy, name string) ([]byte, error) {
		var state guardState
		if name != "runner-one" || json.Unmarshal(g.raw, &state) != nil || isNull(state.Active) || !bytes.Contains(state.Intent, []byte(`"command":"DeleteInstance"`)) {
			t.Fatal("delete lacks original target/intent")
		}
		if raw, err := os.ReadFile(filepath.Join(s.state, "state.json")); err != nil || !bytes.Equal(raw, g.raw) {
			t.Fatal("intent not persisted before deletion")
		}
		if _, err := s.check(); !errors.Is(err, errBusy) {
			t.Fatal("permanent lock not held")
		}
		return []byte("\n"), nil
	}
	return s, env, d
}

func TestDeleteReleasesCapacity(t *testing.T) {
	for _, mode := range []string{"ready", "receipt", "cleanup-latch"} {
		t.Run(mode, func(t *testing.T) {
			s, env, d := deleteFixture(t)
			path := filepath.Join(s.state, "state.json")
			raw, _ := os.ReadFile(path)
			if mode == "receipt" {
				raw = bytes.Replace(raw, []byte(`"ready":true,"agent_id":42`), []byte(`"ready":false`), 1)
			}
			if mode == "cleanup-latch" {
				raw = bytes.Replace(raw, []byte(`"latch":null`), []byte(`"latch":{"reason":"Low capacity","at":1793000000,"blocks_delete":false}`), 1)
			}
			write(t, path, raw, 0600)
			var original guardState
			json.Unmarshal(raw, &original)
			d.after = func(context.Context) error {
				raw, _ := os.ReadFile(path)
				var state guardState
				json.Unmarshal(raw, &state)
				if isNull(state.Active) || isNull(state.Intent) {
					t.Fatal("capacity released before vacancy observation")
				}
				return nil
			}
			if stage, err := s.delete(context.Background(), env, d); err != nil {
				t.Fatalf("%s: %v", stage, err)
			}
			raw, _ = os.ReadFile(path)
			var state guardState
			json.Unmarshal(raw, &state)
			if !isNull(state.Active) || !isNull(state.Intent) || !bytes.Equal(state.Latch, original.Latch) {
				t.Fatal("release/latch state differs")
			}
			d.invoke = func(context.Context, *guardFiles, deletePolicy, string) ([]byte, error) {
				t.Fatal("delete replayed")
				return nil, nil
			}
			if _, err := s.delete(context.Background(), env, d); err == nil {
				t.Fatal("completed deletion replay accepted")
			}
			// Reuse the actual creation state machine to prove capacity admission.
			_, _, createEnv, input, c := createFixture(t)
			createEnv["GARM_PROVIDER_CONFIG_FILE"] = s.config
			c.invoke = func(context.Context, *guardFiles, createPolicy, []byte) ([]byte, error) {
				return []byte(`{"name":"runner-one","provider_id":"runner-one","status":"running","os_type":"linux","os_arch":"amd64"}`), nil
			}
			_, _, err := s.create(context.Background(), createEnv, bytes.NewReader(input), c)
			if (err == nil) != (mode != "cleanup-latch") {
				t.Fatalf("create admission after release: %v", err)
			}
		})
	}
}

func TestDeleteAdmissionPreservesOriginal(t *testing.T) {
	for _, mode := range []string{"command", "controller", "pool", "config", "interface", "name", "missing-name", "absent-active", "intent", "latch", "old-latch", "null-block", "maintenance", "not-ready", "wrong-agent", "observation", "persist"} {
		t.Run(mode, func(t *testing.T) {
			s, env, d := deleteFixture(t)
			path := filepath.Join(s.state, "state.json")
			raw, _ := os.ReadFile(path)
			switch mode {
			case "command":
				env["GARM_COMMAND"] = "RemoveAllInstances"
			case "controller":
				env["GARM_CONTROLLER_ID"] = poolID
			case "pool":
				env["GARM_POOL_ID"] = controllerID
			case "config":
				env["GARM_PROVIDER_CONFIG_FILE"] = "/other"
			case "interface":
				env["GARM_INTERFACE_VERSION"] = "v0.1.1"
			case "name":
				env["GARM_INSTANCE_ID"] = "another-vm"
			case "missing-name":
				delete(env, "GARM_INSTANCE_ID")
			case "absent-active":
				raw = []byte(clearState)
			case "intent":
				raw = bytes.Replace(raw, []byte(`"intent":null`), []byte(`"intent":{"command":"DeleteInstance","name":"runner-one","at":1793000000}`), 1)
			case "latch", "old-latch", "null-block":
				field := `,"blocks_delete":true`
				if mode == "old-latch" {
					field = ""
				}
				if mode == "null-block" {
					field = `,"blocks_delete":null`
				}
				raw = bytes.Replace(raw, []byte(`"latch":null`), []byte(`"latch":{"reason":"Retain","at":1793000000`+field+`}`), 1)
			case "maintenance":
				write(t, s.maintenance, []byte("retain"), 0600)
			case "not-ready":
				d.before = func(context.Context, guardPolicy, activeInstance) (*int64, error) { return nil, nil }
			case "wrong-agent":
				d.before = func(context.Context, guardPolicy, activeInstance) (*int64, error) { id := int64(99); return &id, nil }
			case "observation":
				d.before = func(context.Context, guardPolicy, activeInstance) (*int64, error) { return nil, errRefused }
			case "persist":
				d.before = func(context.Context, guardPolicy, activeInstance) (*int64, error) {
					write(t, filepath.Join(s.state, "state.new"), []byte("collision"), 0600)
					id := int64(42)
					return &id, nil
				}
			}
			write(t, path, raw, 0600)
			d.invoke = func(context.Context, *guardFiles, deletePolicy, string) ([]byte, error) {
				t.Fatal("unadmitted deletion")
				return nil, nil
			}
			if _, err := s.delete(context.Background(), env, d); err == nil {
				t.Fatal("admitted")
			}
			if current, _ := os.ReadFile(path); !bytes.Equal(current, raw) {
				t.Fatal("original state changed")
			}
		})
	}
}

func TestDeleteUnknownNeverReplaysOrReleases(t *testing.T) {
	for _, mode := range []string{"native", "response", "vacancy", "cancel", "commit"} {
		t.Run(mode, func(t *testing.T) {
			s, env, d := deleteFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			invoke := d.invoke
			d.invoke = func(ctx context.Context, g *guardFiles, p deletePolicy, name string) ([]byte, error) {
				out, err := invoke(ctx, g, p, name)
				switch mode {
				case "native":
					return nil, errors.New("lost response")
				case "response":
					return []byte("unexpected response"), nil
				case "cancel":
					cancel()
				case "commit":
					write(t, filepath.Join(s.state, "state.pending"), []byte("retain"), 0600)
				}
				return out, err
			}
			if mode == "vacancy" {
				d.after = func(context.Context) error { return errRefused }
			}
			if _, err := s.delete(ctx, env, d); err == nil {
				t.Fatal("uncertain delete succeeded")
			}
			raw, _ := os.ReadFile(filepath.Join(s.state, "state.json"))
			var state guardState
			json.Unmarshal(raw, &state)
			if isNull(state.Active) || isNull(state.Intent) || !strings.Contains(string(state.Intent), "DeleteInstance") {
				t.Fatal("original delete facts lost")
			}
			d.invoke = func(context.Context, *guardFiles, deletePolicy, string) ([]byte, error) {
				t.Fatal("unknown delete replayed")
				return nil, nil
			}
			if _, err := s.delete(context.Background(), env, d); err == nil {
				t.Fatal("unknown admitted")
			}
		})
	}
}
