package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const createSpecs = `{"disable_updates":true,"runner_install_template":"aGVsbG8K"}`

func createFixture(t *testing.T) (startup, createPolicy, map[string]string, []byte, creator) {
	t.Helper()
	s := fixture(t)
	p := createPolicy{Controller: controllerID, Pool: poolID, Image: strings.Repeat("a", 64), Alias: "runner-image", Specs: fmt.Sprintf("%x", sha256.Sum256([]byte(createSpecs))), ProviderSHA: fileSHA(t, s.native), ConfigSHA: fileSHA(t, s.config)}
	raw, _ := json.Marshal(p)
	write(t, s.policy, raw, 0644)
	env := map[string]string{"GARM_COMMAND": "CreateInstance", "GARM_CONTROLLER_ID": p.Controller, "GARM_POOL_ID": p.Pool, "GARM_PROVIDER_CONFIG_FILE": s.config, "GARM_INTERFACE_VERSION": "v0.1.0"}
	input := []byte(fmt.Sprintf(`{"name":"runner-one","image":"runner-image","flavor":"halo-ci","os_type":"linux","arch":"amd64","pool_id":%q,"jit_config_enabled":true,"repo_url":"https://github.com/silence-force/nucleus","callback-url":"https://10.203.64.1:9443/api/v1/callbacks","metadata-url":"https://10.203.64.1:9443/api/v1/metadata","instance-token":"synthetic-secret-never-print","extra_specs":%s}`, p.Pool, createSpecs))
	c := creator{capacity: func(context.Context) error { return nil }, before: func(context.Context, createPolicy) error { return nil }, after: func(context.Context, createPolicy, string) (string, error) { return controllerID, nil }, now: func() time.Time { return time.Unix(1793000000, 0) }}
	c.invoke = func(_ context.Context, g *guardFiles, _ createPolicy, got []byte) ([]byte, error) {
		if !bytes.Equal(got, input) {
			t.Fatal("bootstrap bytes changed")
		}
		var state guardState
		if json.Unmarshal(g.raw, &state) != nil || isNull(state.Intent) {
			t.Fatal("native before intent")
		}
		if raw, err := os.ReadFile(filepath.Join(s.state, "state.json")); err != nil || !bytes.Equal(raw, g.raw) {
			t.Fatal("intent not on disk")
		}
		if stage, err := s.check(); !errors.Is(err, errBusy) || stage != "busy" {
			t.Fatal("native not protected by permanent lock")
		}
		return []byte(`{"name":"runner-one","provider_id":"runner-one","status":"running","os_type":"linux","os_arch":"amd64"}`), nil
	}
	return s, p, env, input, c
}

func TestCreateUpdatePolicyComesFromSpecs(t *testing.T) {
	_, p, _, input, _ := createFixture(t)
	// Native v0.1.5 getCreateInstanceArgs overwrites this option from extra_specs.
	for _, option := range []string{"null", `{}`, `{"disable_updates_on_boot":false}`, `{"disable_updates_on_boot":true}`} {
		raw := []byte(strings.TrimSuffix(string(input), "}") + `,"user_data_options":` + option + `}`)
		if _, err := createInput(raw, p); err != nil {
			t.Fatalf("overwritten option rejected: %s", option)
		}
		raw = bytes.Replace(raw, []byte(`"disable_updates":true`), []byte(`"disable_updates":false`), 1)
		if _, err := createInput(raw, p); err == nil {
			t.Fatal("unapproved update policy accepted")
		}
	}
}

func TestCreatePersistencePrecedesExecution(t *testing.T) {
	s, _, env, input, c := createFixture(t)
	c.before = func(context.Context, createPolicy) error {
		write(t, filepath.Join(s.state, "state.new"), []byte("preserve-collision"), 0600)
		return nil
	}
	c.invoke = func(context.Context, *guardFiles, createPolicy, []byte) ([]byte, error) {
		t.Fatal("native ran without durable intent")
		return nil, nil
	}
	if _, _, err := s.create(context.Background(), env, bytes.NewReader(input), c); err == nil {
		t.Fatal("collision accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(s.state, "state.new")); string(got) != "preserve-collision" {
		t.Fatal("original collision overwritten")
	}
}

func TestCreateCancellationReleasesInput(t *testing.T) {
	s, _, env, _, c := createFixture(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := s.create(ctx, env, reader, c); done <- err }()
	// Write synchronizes with the command's read, then leave the request incomplete.
	if _, err := writer.Write([]byte("{")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("input cancellation ignored")
	}
	if fileSHA(t, filepath.Join(s.state, "state.json")) != fmt.Sprintf("%x", sha256.Sum256([]byte(clearState))) {
		t.Fatal("cancelled read wrote intent")
	}
}

func TestCreateCrashHelper(t *testing.T) {
	base := os.Getenv("GUARD_CREATE_HELPER")
	if base == "" {
		return
	}
	s := startup{policy: filepath.Join(base, "policy.json"), native: filepath.Join(base, "native"), config: filepath.Join(base, "config.toml"), state: filepath.Join(base, "state"), maintenance: filepath.Join(base, "maintenance"), rootUID: uint32(os.Geteuid()), serviceUID: uint32(os.Geteuid())}
	raw, err := os.ReadFile(s.policy)
	if err != nil {
		t.Fatal(err)
	}
	var p createPolicy
	if json.Unmarshal(raw, &p) != nil {
		t.Fatal("policy")
	}
	input, err := os.ReadFile(filepath.Join(base, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GARM_COMMAND": "CreateInstance", "GARM_CONTROLLER_ID": p.Controller, "GARM_POOL_ID": p.Pool, "GARM_PROVIDER_CONFIG_FILE": s.config, "GARM_INTERFACE_VERSION": "v0.1.0"}
	c := creator{capacity: func(context.Context) error { return nil }, before: func(context.Context, createPolicy) error { return nil }, after: func(context.Context, createPolicy, string) (string, error) { return "", errRefused }, invoke: invokeCreate, now: time.Now}
	if os.Getenv("GUARD_WRITE_COMMAND") == "DeleteInstance" {
		env["GARM_COMMAND"], env["GARM_INSTANCE_ID"] = "DeleteInstance", "runner-one"
		d := deleter{before: func(context.Context, guardPolicy, activeInstance) (*int64, error) { id := int64(42); return &id, nil }, after: func(context.Context) error { return errRefused }, invoke: invokeDelete, now: time.Now}
		_, err = s.delete(context.Background(), env, d)
		t.Fatalf("delete worker unexpectedly returned: %v", err)
	}
	_, _, err = s.create(context.Background(), env, bytes.NewReader(input), c)
	t.Fatalf("worker unexpectedly returned: %v", err)
}

func TestCreateSurvivesGuardDeathWithoutReplay(t *testing.T) {
	testWriteSurvivesGuardDeath(t, "CreateInstance")
}

func TestDeleteSurvivesGuardDeathWithoutReplay(t *testing.T) {
	testWriteSurvivesGuardDeath(t, "DeleteInstance")
}

func testWriteSurvivesGuardDeath(t *testing.T, command string) {
	t.Helper()
	s, p, env, input, c := createFixture(t)
	var d deleter
	if command == "DeleteInstance" {
		s, env, d = deleteFixture(t)
	}
	base := filepath.Dir(s.native)
	fifo := filepath.Join(base, "entered")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	pipe, err := os.OpenFile(fifo, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$$\" > '%s'\nexec /bin/sleep 60\n", fifo)
	write(t, s.native, []byte(script), 0755)
	p.ProviderSHA = fileSHA(t, s.native)
	raw, _ := json.Marshal(p)
	write(t, s.policy, raw, 0644)
	write(t, filepath.Join(base, "input.json"), input, 0600)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	worker := exec.CommandContext(ctx, exe, "-test.run=^TestCreateCrashHelper$")
	worker.Env = append(os.Environ(), "GUARD_CREATE_HELPER="+base, "GUARD_WRITE_COMMAND="+command)
	var diagnostic bytes.Buffer
	worker.Stderr = &diagnostic
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if worker.ProcessState == nil {
			worker.Process.Kill()
			worker.Wait()
		}
	}()
	entered := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(pipe).ReadString('\n'); entered <- line }()
	var line string
	select {
	case line = <-entered:
	case <-ctx.Done():
		t.Fatal("native did not start")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		t.Fatal("bad child identity")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := worker.Wait(); err == nil {
		t.Fatal("guard not killed")
	}
	if n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 3000); err != nil || n != 1 {
		t.Fatal("native survived guard death")
	}
	raw, err = os.ReadFile(filepath.Join(s.state, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state guardState
	json.Unmarshal(raw, &state)
	if isNull(state.Intent) || !bytes.Contains(state.Intent, []byte("runner-one")) {
		t.Fatal("original intent lost")
	}
	if command == "DeleteInstance" {
		if isNull(state.Active) {
			t.Fatal("killed delete released capacity")
		}
		d.invoke = func(context.Context, *guardFiles, deletePolicy, string) ([]byte, error) {
			t.Fatal("killed delete replayed")
			return nil, nil
		}
		if _, err := s.delete(context.Background(), env, d); err == nil {
			t.Fatal("unknown delete admitted")
		}
		return
	}
	c.invoke = func(context.Context, *guardFiles, createPolicy, []byte) ([]byte, error) {
		t.Fatal("killed operation replayed")
		return nil, nil
	}
	if _, _, err := s.create(context.Background(), env, bytes.NewReader(input), c); err == nil {
		t.Fatal("unknown operation admitted")
	}
}

func TestCreateCommitsActiveAndNeverRepeats(t *testing.T) {
	s, _, env, input, c := createFixture(t)
	stage, out, err := s.create(context.Background(), env, bytes.NewReader(input), c)
	if err != nil || len(out) == 0 {
		t.Fatalf("stage=%s err=%v", stage, err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.state, "state.json"))
	var state guardState
	json.Unmarshal(raw, &state)
	var active activeInstance
	json.Unmarshal(state.Active, &active)
	if !isNull(state.Intent) || active.Name != "runner-one" || active.UUID != controllerID || active.Ready == nil || *active.Ready {
		t.Fatal("wrong committed active")
	}
	c.invoke = func(context.Context, *guardFiles, createPolicy, []byte) ([]byte, error) {
		t.Fatal("repeated native")
		return nil, nil
	}
	if _, _, err := s.create(context.Background(), env, bytes.NewReader(input), c); err == nil {
		t.Fatal("occupied capacity reused")
	}
}

func TestCreateRejectsRequestBeforeNative(t *testing.T) {
	for _, mutation := range []string{"command", "controller", "pool", "config", "interface", "duplicate", "image", "specs", "missing-jit", "null", "unknown", "oversized", "active", "intent", "latch", "capacity", "inventory"} {
		t.Run(mutation, func(t *testing.T) {
			s, _, env, input, c := createFixture(t)
			switch mutation {
			case "command":
				env["GARM_COMMAND"] = "DeleteInstance"
			case "controller":
				env["GARM_CONTROLLER_ID"] = "other"
			case "pool":
				env["GARM_POOL_ID"] = "other"
			case "config":
				env["GARM_PROVIDER_CONFIG_FILE"] = "other"
			case "interface":
				env["GARM_INTERFACE_VERSION"] = "v0.1.1"
			case "duplicate":
				input = bytes.Replace(input, []byte(`"name":"runner-one"`), []byte(`"name":"other","name":"runner-one"`), 1)
			case "image":
				input = bytes.ReplaceAll(input, []byte("runner-image"), []byte("other-image"))
			case "specs":
				input = bytes.ReplaceAll(input, []byte("aGVsbG8K"), []byte("d29ybGQK"))
			case "missing-jit":
				input = bytes.ReplaceAll(input, []byte(`"jit_config_enabled":true,`), nil)
			case "null":
				input = []byte("null")
			case "unknown":
				input = append([]byte(`{"injected":true,`), input[1:]...)
			case "oversized":
				input = []byte(strings.Repeat(" ", 2*1024*1024+1))
			case "active", "intent", "latch":
				write(t, filepath.Join(s.state, "state.json"), []byte(strings.Replace(clearState, `"`+mutation+`":null`, `"`+mutation+`":{}`, 1)), 0600)
			case "capacity":
				c.capacity = func(context.Context) error { return errRefused }
			case "inventory":
				c.before = func(context.Context, createPolicy) error { return errRefused }
			}
			before := fileSHA(t, filepath.Join(s.state, "state.json"))
			c.invoke = func(context.Context, *guardFiles, createPolicy, []byte) ([]byte, error) {
				t.Fatal("refused request executed")
				return nil, nil
			}
			if _, out, err := s.create(context.Background(), env, bytes.NewReader(input), c); err == nil || len(out) != 0 {
				t.Fatal("request accepted")
			}
			if fileSHA(t, filepath.Join(s.state, "state.json")) != before {
				t.Fatal("preflight changed state")
			}
		})
	}
}

func TestCreateFailureKeepsIntent(t *testing.T) {
	for _, failure := range []string{"native", "response", "identity", "cancel", "commit"} {
		t.Run(failure, func(t *testing.T) {
			s, _, env, input, c := createFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			invoke := c.invoke
			c.invoke = func(ctx context.Context, g *guardFiles, p createPolicy, raw []byte) ([]byte, error) {
				result, err := invoke(ctx, g, p, raw)
				switch failure {
				case "native":
					return nil, errors.New("synthetic native failure")
				case "response":
					return []byte(`{"name":"other","provider_id":"other"}`), nil
				case "cancel":
					cancel()
				case "commit":
					write(t, filepath.Join(s.state, "state.pending"), []byte("retain-original"), 0600)
				}
				return result, err
			}
			if failure == "identity" {
				c.after = func(context.Context, createPolicy, string) (string, error) { return "", errRefused }
			}
			if _, out, err := s.create(ctx, env, bytes.NewReader(input), c); err == nil || len(out) != 0 {
				t.Fatal("failure reported success")
			}
			raw, _ := os.ReadFile(filepath.Join(s.state, "state.json"))
			var state guardState
			json.Unmarshal(raw, &state)
			if isNull(state.Intent) || !isNull(state.Active) || bytes.Contains(raw, []byte("synthetic-secret")) {
				t.Fatal("intent lost or secret persisted")
			}
			c.invoke = func(context.Context, *guardFiles, createPolicy, []byte) ([]byte, error) {
				t.Fatal("unknown outcome replayed")
				return nil, nil
			}
			if _, _, err := s.create(context.Background(), env, bytes.NewReader(input), c); err == nil {
				t.Fatal("unknown admitted")
			}
		})
	}
}

func TestCreateNativeUsesPinnedInputsAndWaits(t *testing.T) {
	testNativeUsesPinnedInputsAndWaits(t, "CreateInstance")
}

func TestDeleteNativeUsesPinnedInputsAndWaits(t *testing.T) {
	testNativeUsesPinnedInputsAndWaits(t, "DeleteInstance")
}

func testNativeUsesPinnedInputsAndWaits(t *testing.T, command string) {
	t.Helper()
	for _, mode := range []string{"success", "exit", "cancel", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			s, p, env, input, c := createFixture(t)
			var d deleter
			if command == "DeleteInstance" {
				s, env, d = deleteFixture(t)
				d.invoke = invokeDelete
			}
			fifo := filepath.Join(filepath.Dir(s.native), "entered")
			if err := unix.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			pipe, err := os.OpenFile(fifo, os.O_RDWR|unix.O_NONBLOCK, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer pipe.Close()
			script := fmt.Sprintf("#!/bin/sh\n[ \"$GARM_COMMAND\" = %s ] || exit 2\n[ \"$GARM_PROVIDER_CONFIG_FILE\" = /proc/self/fd/4 ] || exit 3\n", command)
			script += "IFS= read -r config < \"$GARM_PROVIDER_CONFIG_FILE\"\n[ \"$config\" = \"secret = 'never-log-this'\" ] || exit 4\n/bin/cat > /dev/null\n"
			if command == "DeleteInstance" {
				script += "[ \"$GARM_INSTANCE_ID\" = runner-one ] || exit 5\n[ -z \"$GARM_POOL_ID\" ] || exit 6\n"
			}
			script += fmt.Sprintf("printf ready > '%s'\n", fifo)
			switch mode {
			case "success":
				if command == "DeleteInstance" {
					script += "printf '\\n'\n"
				} else {
					script += "printf '{\"name\":\"runner-one\",\"provider_id\":\"runner-one\",\"status\":\"running\",\"os_type\":\"linux\",\"os_arch\":\"amd64\"}'\n"
				}
			case "exit":
				script += "printf secret >&2\nexit 9\n"
			case "cancel":
				script += "exec /bin/sleep 60\n"
			case "overflow":
				script += "/bin/dd if=/dev/zero bs=1048576 count=3 2>/dev/null\nexit 0\n"
			}
			write(t, s.native, []byte(script), 0755)
			p.ProviderSHA = fileSHA(t, s.native)
			raw, _ := json.Marshal(p)
			write(t, s.policy, raw, 0644)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c.invoke = invokeCreate
			type result struct {
				out []byte
				err error
			}
			done := make(chan result, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if command == "DeleteInstance" {
					_, err := s.delete(ctx, env, d)
					done <- result{nil, err}
					return
				}
				_, out, err := s.create(ctx, env, bytes.NewReader(input), c)
				done <- result{out, err}
			}()
			defer func() { cancel(); <-finished }()
			entered := make(chan error, 1)
			go func() { _, err := io.ReadFull(pipe, make([]byte, 5)); entered <- err }()
			select {
			case err := <-entered:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("native did not consume pinned inputs")
			}
			if mode == "cancel" {
				cancel()
			}
			got := <-done
			out, err := got.out, got.err
			if command == "CreateInstance" && mode == "success" && !bytes.Contains(out, []byte("runner-one")) {
				t.Fatal("native output lost")
			}
			if (err == nil) != (mode == "success") {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
			if mode != "cancel" && ctx.Err() != nil {
				t.Fatal("deadline masked the intended result")
			}
			var exit *exec.ExitError
			if mode == "exit" && (!errors.As(err, &exit) || exit.ExitCode() != 9) {
				t.Fatalf("wrong exit result: %v", err)
			}
			if mode == "overflow" && !errors.Is(err, errRefused) {
				t.Fatalf("output limit not reached: %v", err)
			}
			if mode == "cancel" && (!errors.As(err, &exit) || ctx.Err() != context.Canceled) {
				t.Fatalf("running native not cancelled: %v", err)
			}
			if command == "DeleteInstance" {
				raw, _ := os.ReadFile(filepath.Join(s.state, "state.json"))
				var state guardState
				json.Unmarshal(raw, &state)
				if (isNull(state.Active) && isNull(state.Intent)) != (mode == "success") {
					t.Fatal("native result lost capacity state")
				}
			}
		})
	}
}
