package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScaleSetPoolAdmission(t *testing.T) {
	// GARM v0.2.1 builds the provider pool from the ScaleSet name and entity ID.
	const repository = "33333333-2222-4333-8444-555555555555"
	const pool = "nucleiron-halo-33333333-2222-4333-8444-555555555555"
	for _, command := range []string{"watchdog", "CreateInstance", "DeleteInstance", "GetVersion", "GetInstance", "ListInstances"} {
		t.Run(command, func(t *testing.T) {
			var s startup
			var run func() error
			switch command {
			case "watchdog":
				var h health
				s, h = watchdogFixture(t)
				run = func() error { _, err := s.watchdog(context.Background(), h); return err }
			case "CreateInstance":
				var env map[string]string
				var input []byte
				var c creator
				s, _, env, input, c = createFixture(t)
				env["GARM_POOL_ID"] = pool
				input = bytes.ReplaceAll(input, []byte(poolID), []byte(pool))
				c.invoke = func(_ context.Context, _ *guardFiles, _ createPolicy, raw []byte) ([]byte, error) {
					if !bytes.Equal(raw, input) {
						t.Fatal("ScaleSet request changed")
					}
					return []byte(readInstance), nil
				}
				run = func() error { _, _, err := s.create(context.Background(), env, bytes.NewReader(input), c); return err }
			case "DeleteInstance":
				var env map[string]string
				var d deleter
				s, env, d = deleteFixture(t)
				run = func() error { _, err := s.delete(context.Background(), env, d); return err }
			default:
				var env map[string]string
				var r reader
				s, env, r = readFixture(t, command)
				if command == "ListInstances" {
					env["GARM_POOL_ID"] = pool
				}
				r.bound = func(_ context.Context, p guardPolicy, _ activeInstance) error {
					if p.Pool != pool {
						t.Fatal("ScaleSet identity changed")
					}
					return nil
				}
				run = func() error { _, _, err := s.read(context.Background(), env, r); return err }
			}
			raw, err := os.ReadFile(s.policy)
			if err != nil {
				t.Fatal(err)
			}
			var policy map[string]any
			if err := json.Unmarshal(raw, &policy); err != nil {
				t.Fatal(err)
			}
			policy["pool_id"], policy["repository_id"], policy["scaleset_name"] = pool, repository, "nucleiron-halo"
			raw, err = json.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			write(t, s.policy, raw, 0644)
			if err := run(); err != nil {
				t.Fatalf("bound ScaleSet rejected: %v", err)
			}
		})
	}
}

func TestScaleSetPoolRejectsUnboundPolicy(t *testing.T) {
	for _, command := range []string{"watchdog", "CreateInstance", "DeleteInstance", "GetVersion", "GetInstance", "ListInstances"} {
		for _, mode := range []string{"uuid-pool", "wrong-repository", "wrong-scaleset", "missing-repository", "missing-scaleset", "null-repository", "number-scaleset", "empty-pool", "nul-name"} {
			t.Run(command+"/"+mode, func(t *testing.T) {
				var s startup
				var run func() error
				switch command {
				case "watchdog":
					var h health
					s, h = watchdogFixture(t)
					h.capacity = func(context.Context) error { t.Fatal("observation before policy admission"); return nil }
					run = func() error { _, err := s.watchdog(context.Background(), h); return err }
				case "CreateInstance":
					var env map[string]string
					var input []byte
					var c creator
					s, _, env, input, c = createFixture(t)
					c.capacity = func(context.Context) error { t.Fatal("creation before policy admission"); return nil }
					run = func() error { _, _, err := s.create(context.Background(), env, bytes.NewReader(input), c); return err }
				case "DeleteInstance":
					var env map[string]string
					var d deleter
					s, env, d = deleteFixture(t)
					d.before = func(context.Context, guardPolicy, activeInstance) (*int64, error) {
						t.Fatal("deletion before policy admission")
						return nil, nil
					}
					run = func() error { _, err := s.delete(context.Background(), env, d); return err }
				default:
					var env map[string]string
					var r reader
					s, env, r = readFixture(t, command)
					r.invoke = func(context.Context, *guardFiles, readPolicy, string, string) ([]byte, error) {
						t.Fatal("native before policy admission")
						return nil, nil
					}
					run = func() error { _, _, err := s.read(context.Background(), env, r); return err }
				}
				raw, err := os.ReadFile(s.policy)
				if err != nil {
					t.Fatal(err)
				}
				var policy map[string]any
				if err := json.Unmarshal(raw, &policy); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "uuid-pool":
					policy["pool_id"] = repositoryID
				case "wrong-repository":
					policy["repository_id"] = controllerID
				case "wrong-scaleset":
					policy["scaleset_name"] = "another-set"
				case "missing-repository":
					delete(policy, "repository_id")
				case "missing-scaleset":
					delete(policy, "scaleset_name")
				case "null-repository":
					policy["repository_id"] = nil
				case "number-scaleset":
					policy["scaleset_name"] = 123
				case "empty-pool":
					policy["pool_id"] = ""
				case "nul-name":
					policy["scaleset_name"], policy["pool_id"] = "set\x00", "set\x00-"+repositoryID
				}
				raw, err = json.Marshal(policy)
				if err != nil {
					t.Fatal(err)
				}
				write(t, s.policy, raw, 0644)
				original, err := os.ReadFile(filepath.Join(s.state, "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := run(); err == nil {
					t.Fatal("unbound policy admitted")
				}
				assertReadState(t, s, original)
			})
		}
	}
}
