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

const readInstance = `{"name":"runner-one","provider_id":"runner-one","status":"running","os_type":"linux","os_arch":"amd64"}`

func readFixture(t *testing.T, command string) (startup, map[string]string, reader) {
	t.Helper()
	s, env, _ := deleteFixture(t)
	env["GARM_COMMAND"] = command
	if command != "GetInstance" {
		delete(env, "GARM_INSTANCE_ID")
	}
	if command == "ListInstances" {
		env["GARM_POOL_ID"] = poolID
	}
	r := reader{
		bound: func(_ context.Context, p guardPolicy, a activeInstance) error {
			if p.Controller != controllerID || p.Pool != poolID || a.Name != "runner-one" || a.UUID != controllerID {
				t.Fatal("wrong observation identity")
			}
			return nil
		},
		vacant: func(context.Context) error { t.Fatal("active instance treated as vacant"); return nil },
	}
	r.invoke = func(_ context.Context, _ *guardFiles, _ readPolicy, got, name string) ([]byte, error) {
		if got != command || name != env["GARM_INSTANCE_ID"] {
			t.Fatal("wrong native command")
		}
		if _, err := s.check(); !errors.Is(err, errBusy) {
			t.Fatal("read lacks permanent lock")
		}
		switch command {
		case "GetVersion":
			return []byte("v0.1.5\n"), nil
		case "ListInstances":
			return []byte("[" + readInstance + "]"), nil
		default:
			return []byte(readInstance), nil
		}
	}
	return s, env, r
}

func assertReadState(t *testing.T, s startup, original []byte) {
	t.Helper()
	current, err := os.ReadFile(filepath.Join(s.state, "state.json"))
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("read changed guard state")
	}
	for _, name := range []string{"state.pending", "state.new"} {
		if _, err := os.Stat(filepath.Join(s.state, name)); !os.IsNotExist(err) {
			t.Fatal("read created transaction material")
		}
	}
}

func TestReadPreservesFactsAndNativeResponse(t *testing.T) {
	for _, command := range []string{"GetVersion", "GetInstance", "ListInstances"} {
		for _, ready := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ready=%t", command, ready), func(t *testing.T) {
				s, env, r := readFixture(t, command)
				path := filepath.Join(s.state, "state.json")
				raw, _ := os.ReadFile(path)
				if !ready {
					raw = bytes.Replace(raw, []byte(`"ready":true,"agent_id":42`), []byte(`"ready":false`), 1)
					write(t, path, raw, 0600)
				}
				want := []byte(readInstance)
				if command == "ListInstances" {
					want = []byte("[" + readInstance + "]")
				}
				if command == "GetVersion" {
					want = []byte("v0.1.5\n")
					r.bound = func(context.Context, guardPolicy, activeInstance) error {
						t.Fatal("version read observes VM")
						return nil
					}
				}
				stage, got, err := s.read(context.Background(), env, r)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s: %v; response %q", stage, err, got)
				}
				assertReadState(t, s, raw)
			})
		}
	}
}

func TestReadAdmission(t *testing.T) {
	for _, mode := range []string{"command", "controller", "pool", "config", "interface", "name", "missing-name", "absent-active", "intent", "latch", "maintenance", "list-pool", "list-name", "version-name", "digest"} {
		t.Run(mode, func(t *testing.T) {
			s, env, r := readFixture(t, "GetInstance")
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
				env["GARM_INTERFACE_VERSION"] = "v0.2.0"
			case "name":
				env["GARM_INSTANCE_ID"] = "other"
			case "missing-name":
				delete(env, "GARM_INSTANCE_ID")
			case "absent-active":
				raw = []byte(clearState)
			case "intent":
				raw = bytes.Replace(raw, []byte(`"intent":null`), []byte(`"intent":{"command":"DeleteInstance","name":"runner-one","at":1793000000}`), 1)
			case "latch":
				raw = bytes.Replace(raw, []byte(`"latch":null`), []byte(`"latch":{"reason":"Retain","at":1793000000,"blocks_delete":false}`), 1)
			case "maintenance":
				write(t, s.maintenance, []byte("retain"), 0600)
			case "list-pool":
				env["GARM_COMMAND"] = "ListInstances"
				delete(env, "GARM_INSTANCE_ID")
			case "list-name":
				env["GARM_COMMAND"], env["GARM_POOL_ID"] = "ListInstances", poolID
			case "version-name":
				env["GARM_COMMAND"] = "GetVersion"
			case "digest":
				write(t, s.native, []byte("changed"), 0755)
			}
			write(t, path, raw, 0600)
			r.invoke = func(context.Context, *guardFiles, readPolicy, string, string) ([]byte, error) {
				t.Fatal("native before admission")
				return nil, nil
			}
			if _, got, err := s.read(context.Background(), env, r); err == nil || len(got) != 0 {
				t.Fatal("unadmitted response returned")
			}
			assertReadState(t, s, raw)
		})
	}
}

func TestReadResponses(t *testing.T) {
	for _, command := range []string{"GetInstance", "ListInstances"} {
		for _, mode := range []string{"running", "stopped", "error", "unknown", "missing-status", "pending", "name", "provider", "os", "arch", "duplicate", "unknown-field", "null", "empty", "multiple", "trailing", "native-error", "identity-error", "cancel"} {
			t.Run(command+"/"+mode, func(t *testing.T) {
				s, env, r := readFixture(t, command)
				original, _ := os.ReadFile(filepath.Join(s.state, "state.json"))
				payload := readInstance
				valid := mode == "running" || mode == "stopped" || mode == "error" || mode == "unknown"
				switch mode {
				case "stopped", "error", "unknown", "pending":
					payload = strings.Replace(payload, `"running"`, fmt.Sprintf("%q", mode), 1)
				case "missing-status":
					payload = strings.Replace(payload, `"status":"running",`, "", 1)
				case "name":
					payload = strings.Replace(payload, `"name":"runner-one"`, `"name":"other"`, 1)
				case "provider":
					payload = strings.Replace(payload, `"provider_id":"runner-one"`, `"provider_id":"other"`, 1)
				case "os":
					payload = strings.Replace(payload, "linux", "windows", 1)
				case "arch":
					payload = strings.Replace(payload, "amd64", "arm64", 1)
				case "duplicate":
					payload = strings.Replace(payload, "{", `{"name":"other",`, 1)
				case "unknown-field":
					payload = strings.Replace(payload, "{", `{"unexpected":true,`, 1)
				case "null":
					payload = "null"
				}
				if command == "ListInstances" {
					payload = "[" + payload + "]"
				}
				if mode == "empty" {
					payload = "[]"
				}
				if mode == "multiple" {
					payload = "[" + readInstance + "," + readInstance + "]"
				}
				if mode == "trailing" {
					payload += " {}"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				r.invoke = func(context.Context, *guardFiles, readPolicy, string, string) ([]byte, error) {
					if mode == "native-error" {
						return []byte(payload), errors.New("native failed")
					}
					return []byte(payload), nil
				}
				r.bound = func(context.Context, guardPolicy, activeInstance) error {
					if mode == "identity-error" {
						return errRefused
					}
					if mode == "cancel" {
						cancel()
					}
					return nil
				}
				stage, got, err := s.read(ctx, env, r)
				if valid {
					if err != nil || string(got) != payload {
						t.Fatalf("%s: %v", stage, err)
					}
				} else if err == nil || len(got) != 0 {
					t.Fatal("invalid response escaped")
				}
				assertReadState(t, s, original)
			})
		}
	}
}

func TestReadVacancyAndVersion(t *testing.T) {
	for _, mode := range []string{"vacant", "leftover", "null-list", "unrecorded-instance", "version", "empty-version", "multiline-version", "control-version"} {
		t.Run(mode, func(t *testing.T) {
			command, payload := "ListInstances", "[]\n"
			if strings.Contains(mode, "version") {
				command, payload = "GetVersion", "v0.0.0-unknown\n"
			}
			s, env, r := readFixture(t, command)
			write(t, filepath.Join(s.state, "state.json"), []byte(clearState), 0600)
			switch mode {
			case "null-list":
				payload = "null"
			case "unrecorded-instance":
				payload = "[" + readInstance + "]"
			case "empty-version":
				payload = " \n"
			case "multiline-version":
				payload = "v0.1.5\nother"
			case "control-version":
				payload = "v0.1.5\x00"
			}
			r.invoke = func(context.Context, *guardFiles, readPolicy, string, string) ([]byte, error) {
				return []byte(payload), nil
			}
			calls := 0
			r.vacant = func(context.Context) error {
				calls++
				if mode == "leftover" {
					return errRefused
				}
				return nil
			}
			_, got, err := s.read(context.Background(), env, r)
			if mode == "vacant" || mode == "version" {
				if err != nil || string(got) != payload {
					t.Fatalf("valid read failed: %v", err)
				}
			} else if err == nil || len(got) != 0 {
				t.Fatal("invalid vacancy/version accepted")
			}
			wantCalls := 0
			if mode == "vacant" || mode == "leftover" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatal("wrong vacancy observation count")
			}
			assertReadState(t, s, []byte(clearState))
		})
	}
}

func TestReadNativeEnvironment(t *testing.T) {
	for _, command := range []string{"GetVersion", "GetInstance", "ListInstances"} {
		t.Run(command, func(t *testing.T) {
			s, env, r := readFixture(t, command)
			original, _ := os.ReadFile(filepath.Join(s.state, "state.json"))
			name, pool, response := "", "", "v0.1.5"
			if command == "GetInstance" {
				name, response = "runner-one", readInstance
			}
			if command == "ListInstances" {
				pool, response = poolID, "["+readInstance+"]"
			}
			program := fmt.Sprintf(`#!/bin/sh
test "$GARM_COMMAND" = '%s' || exit 10
test "$GARM_CONTROLLER_ID" = '%s' || exit 11
test "$GARM_INSTANCE_ID" = '%s' || exit 12
test "$GARM_POOL_ID" = '%s' || exit 13
test "$GARM_INTERFACE_VERSION" = v0.1.0 || exit 14
test "$GARM_PROVIDER_CONFIG_FILE" = /proc/self/fd/4 || exit 15
test -r /proc/self/fd/4 && test -e /proc/self/fd/5 || exit 16
test -z "$READ_SECRET" || exit 17
if read -r input; then exit 18; fi
printf '%%s' '%s'
`, command, controllerID, name, pool, response)
			write(t, s.native, []byte(program), 0755)
			policy, _ := os.ReadFile(s.policy)
			var p createPolicy
			json.Unmarshal(policy, &p)
			p.ProviderSHA = fileSHA(t, s.native)
			policy, _ = json.Marshal(p)
			write(t, s.policy, policy, 0644)
			t.Setenv("READ_SECRET", "synthetic-not-inherited")
			r.invoke = invokeRead
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stage, got, err := s.read(ctx, env, r)
			if err != nil || string(got) != response {
				t.Fatalf("%s: %v", stage, err)
			}
			assertReadState(t, s, original)
		})
	}
}
