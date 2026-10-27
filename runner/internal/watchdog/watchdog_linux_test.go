package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var testBinding = binding{Controller: "11111111-1111-4111-8111-111111111111", Repository: "22222222-2222-4222-8222-222222222222", ScaleSet: "nucleiron-halo"}

type fixture struct {
	mu          sync.Mutex
	o           observer
	log         bytes.Buffer
	requests    []string
	puts, stops int
	enabled     bool
	mode        string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{enabled: true}
	root := t.TempDir()
	f.o = observer{dir: filepath.Join(root, "state"), policy: filepath.Join(root, "policy"), password: filepath.Join(root, "password"), uid: uint32(os.Getuid()), output: &f.log,
		check: func(context.Context) (int, error) { return 1, nil }, stop: func(context.Context) error { f.stops++; return nil }}
	data, _ := json.Marshal(testBinding)
	if err := os.WriteFile(f.o.policy, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.o.password, []byte("synthetic-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.o.api = newController(server.Listener.Addr().String())
	t.Cleanup(f.o.api.client.CloseIdleConnections)
	return f
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		http.Error(w, "unexpected API base", 400)
		return
	}
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/v1")
	w.Header().Set("Content-Type", "application/json")
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.URL.Path != "/auth/login" && r.Header.Get("Authorization") != "Bearer synthetic-session" {
		http.Error(w, "credential mismatch", 401)
		return
	}
	if f.mode == "redirect" {
		w.Header().Set("Location", "/secret-target")
		w.WriteHeader(307)
		return
	}
	if r.URL.Path == "/auth/login" {
		var intent receipt
		data, err := os.ReadFile(filepath.Join(f.o.dir, "state.json"))
		if err != nil || json.Unmarshal(data, &intent) != nil || !intent.LoginPending {
			http.Error(w, "login arrived before durable intent", 400)
			return
		}
		var body map[string]string
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || body["username"] != "haloadmin" || body["password"] != "synthetic-password" {
			http.Error(w, "bad login", 400)
			return
		}
		if f.mode == "login-lost" {
			drop(w)
			return
		}
		if f.mode == "oversize" {
			fmt.Fprint(w, strings.Repeat("x", 2*1024*1024+1))
			return
		}
		if f.mode == "login-trailing" {
			fmt.Fprint(w, `{"token":"synthetic-session"} {}`)
			return
		}
		fmt.Fprint(w, `{"token":"synthetic-session"}`)
		return
	}
	if r.URL.Path == "/controller-info" {
		id := testBinding.Controller
		if f.mode == "wrong-controller" {
			id = testBinding.Repository
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"controller_id": id})
		return
	}
	item := map[string]any{"id": int64(4294967295), "repo_id": testBinding.Repository, "name": testBinding.ScaleSet, "provider_name": "halo-lxd"}
	if f.enabled {
		item["enabled"] = true
	}
	if f.mode == "overflow-id" {
		item["id"] = int64(4294967296)
	}
	if f.mode == "wrong-repository" {
		item["repo_id"] = testBinding.Controller
	}
	if strings.HasPrefix(r.URL.Path, "/repositories/") {
		if r.Method != "GET" || r.URL.Path != "/repositories/"+testBinding.Repository+"/scalesets" {
			http.Error(w, "unexpected", 400)
			return
		}
		switch f.mode {
		case "none":
			fmt.Fprint(w, `[]`)
		case "null-list":
			fmt.Fprint(w, `null`)
		case "null-list-enabled":
			item["enabled"] = nil
			_ = json.NewEncoder(w).Encode([]any{item})
		case "duplicate":
			_ = json.NewEncoder(w).Encode([]any{item, item})
		default:
			_ = json.NewEncoder(w).Encode([]any{item, map[string]any{"id": 44, "name": "unrelated", "repo_id": "unrelated"}})
		}
		return
	}
	if r.URL.Path != "/scalesets/4294967295" {
		http.Error(w, "unexpected target", 400)
		return
	}
	if r.Method == "PUT" {
		var intent receipt
		data, err := os.ReadFile(filepath.Join(f.o.dir, "state.json"))
		if err != nil || json.Unmarshal(data, &intent) != nil || intent.DisablePending != 4294967295 || intent.LoginPending {
			http.Error(w, "PUT arrived before durable intent", 400)
			return
		}
		body, _ := io.ReadAll(r.Body)
		// GARM treats the SDK's optional null pointer fields as unchanged.
		var mutation map[string]any
		if json.Unmarshal(body, &mutation) != nil || len(mutation) != 4 || mutation["enabled"] != false || mutation["enable_shell"] != nil || mutation["state"] != nil || mutation["extended_state"] != nil {
			http.Error(w, "unexpected mutation", 400)
			return
		}
		f.puts++
		if f.mode != "still-enabled" && f.mode != "put-rejected" {
			f.enabled = false
		}
		if f.mode == "put-lost" {
			drop(w)
			return
		}
		if f.mode == "put-rejected" {
			http.Error(w, "private server detail", 500)
			return
		}
		if f.mode == "put-invalid-reply" {
			fmt.Fprint(w, `{`)
			return
		}
		item["enabled"] = f.enabled
		_ = json.NewEncoder(w).Encode(item)
		return
	}
	if f.mode == "provider-drift" {
		item["provider_name"] = "other"
	}
	if f.mode == "missing-enabled" {
		delete(item, "enabled")
	}
	if f.mode == "null-enabled" {
		item["enabled"] = nil
	}
	if f.mode == "fractional-id" {
		item["id"] = 13.5
	}
	if f.mode == "readback-drift" && f.puts > 0 {
		item["repo_id"] = testBinding.Controller
	}
	_ = json.NewEncoder(w).Encode(item)
}

func (f *fixture) run(ctx context.Context) int {
	code := f.o.run(ctx)
	// A dropped HTTP connection can release the client before its server handler
	// returns. Join that handler before inspecting the synthetic server state.
	f.mu.Lock()
	f.mu.Unlock()
	return code
}

func drop(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func TestBoundDisableAndIdleChecks(t *testing.T) {
	for _, code := range []int{0, 75} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := setup(t)
			f.o.check = func(context.Context) (int, error) { return code, nil }
			if got := f.run(context.Background()); got != 0 || len(f.requests) != 0 || f.stops != 0 {
				t.Fatalf("exit=%d requests=%v stops=%d", got, f.requests, f.stops)
			}
		})
	}
	f := setup(t)
	if got := f.run(context.Background()); got != 0 || f.puts != 1 || f.stops != 0 {
		t.Fatalf("exit=%d requests=%v stops=%d", got, f.requests, f.stops)
	}
	want := []string{"POST /auth/login", "GET /controller-info", "GET /repositories/" + testBinding.Repository + "/scalesets", "GET /scalesets/4294967295", "PUT /scalesets/4294967295", "GET /scalesets/4294967295"}
	if fmt.Sprint(f.requests) != fmt.Sprint(want) {
		t.Fatalf("requests=%v", f.requests)
	}
	if got := f.run(context.Background()); got != 0 || f.puts != 1 {
		t.Fatalf("disabled replay exit=%d puts=%d", got, f.puts)
	}
	data, err := readProtected(filepath.Join(f.o.dir, "state.json"), f.o.uid, 0600, 8192)
	var state receipt
	if err != nil || json.Unmarshal(data, &state) != nil || state.DisablePending != 0 || state.LoginPending {
		t.Fatalf("receipt=%s err=%v", data, err)
	}
}

func TestFailuresDoNotModifyAnotherTargetOrRepeatWrites(t *testing.T) {
	for _, mode := range []string{"none", "null-list", "null-list-enabled", "duplicate", "wrong-controller", "wrong-repository", "provider-drift", "missing-enabled", "null-enabled", "fractional-id", "overflow-id", "put-lost", "put-rejected", "put-invalid-reply", "still-enabled", "readback-drift", "login-lost", "login-trailing", "oversize", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.mode = mode
			got := f.run(context.Background())
			if mode == "none" || mode == "missing-enabled" {
				if got != 0 || f.puts != 0 || f.stops != 0 {
					t.Fatalf("exit=%d", got)
				}
				return
			}
			if got != 1 || f.stops != 1 {
				t.Fatalf("exit=%d stops=%d log=%s", got, f.stops, f.log.String())
			}
			wantPuts := 0
			if mode == "put-lost" || mode == "put-rejected" || mode == "put-invalid-reply" || mode == "still-enabled" || mode == "readback-drift" {
				wantPuts = 1
			}
			if f.puts != wantPuts {
				t.Fatalf("puts=%d", f.puts)
			}
			data, err := os.ReadFile(filepath.Join(f.o.dir, "state.json"))
			var pending receipt
			if err != nil || json.Unmarshal(data, &pending) != nil || (wantPuts == 1 && pending.DisablePending != 4294967295) {
				t.Fatalf("lost pending write: %s, %v", data, err)
			}
			requests := len(f.requests)
			if got := f.run(context.Background()); got != 1 || len(f.requests) != requests || f.stops != 1 {
				t.Fatal("repeated an unresolved stop")
			}
			for _, secret := range []string{"synthetic-password", "synthetic-session", "private server detail"} {
				if strings.Contains(f.log.String(), secret) {
					t.Fatal("diagnostics contain secret")
				}
			}
		})
	}
}

func TestOriginalDisableRecoveryOnlyReadsExactID(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			f := setup(t)
			f.enabled = enabled
			lock, err := f.o.lock()
			if err != nil {
				t.Fatal(err)
			}
			lock.Close()
			if err := f.o.save(receipt{Version: 1, Binding: testBinding, DisablePending: 4294967295}); err != nil {
				t.Fatal(err)
			}
			f.o.check = func(context.Context) (int, error) { return 0, nil }
			got := f.run(context.Background())
			if f.puts != 0 || (enabled && got != 1) || (!enabled && got != 0) {
				t.Fatalf("exit=%d puts=%d", got, f.puts)
			}
			for _, req := range f.requests {
				if strings.Contains(req, "/repositories/") {
					t.Fatal("reselected target during recovery")
				}
			}
		})
	}
}

func TestUnknownLoginAndChangedPolicyBlockAuthentication(t *testing.T) {
	for _, mode := range []string{"login", "policy", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			lock, err := f.o.lock()
			if err != nil {
				t.Fatal(err)
			}
			lock.Close()
			state := receipt{Version: 1, Binding: testBinding}
			if mode == "login" {
				state.LoginPending = true
			}
			if mode == "policy" {
				state.Binding.Repository = testBinding.Controller
			}
			if err := f.o.save(state); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				if err := os.WriteFile(filepath.Join(f.o.dir, "state.json"), []byte(`{`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := f.run(context.Background()); got != 1 || len(f.requests) != 0 || f.stops != 1 {
				t.Fatalf("exit=%d requests=%v", got, f.requests)
			}
		})
	}
}

func TestConcurrentTickDoesNotStopActiveObserver(t *testing.T) {
	f := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.o.check = func(context.Context) (int, error) { close(entered); <-release; return 0, nil }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if got := f.run(context.Background()); got != 0 {
			t.Errorf("first exit=%d", got)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("check not entered")
	}
	if got := f.run(context.Background()); got != 75 {
		t.Errorf("concurrent exit=%d", got)
	}
	close(release)
	wg.Wait()
	if f.stops != 0 || len(f.requests) != 0 {
		t.Fatal("busy observer changed admission")
	}
}

func TestProtectedFilesAndStopFailure(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "fifo", "directory", "permissions", "oversize", "stop-failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			if mode == "stop-failure" {
				f.o.stop = func(context.Context) error { f.stops++; return errors.New("private command diagnostic") }
			}
			if mode != "stop-failure" && mode != "cancelled" {
				if err := os.Remove(f.o.password); err != nil {
					t.Fatal(err)
				}
				linkTarget := filepath.Join(filepath.Dir(f.o.password), "valid-password")
				if mode == "symlink" || mode == "hardlink" {
					if err := os.WriteFile(linkTarget, []byte("synthetic-password"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				switch mode {
				case "symlink":
					if err := os.Symlink(linkTarget, f.o.password); err != nil {
						t.Fatal(err)
					}
				case "hardlink":
					if err := os.Link(linkTarget, f.o.password); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(f.o.password, 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(f.o.password, 0600); err != nil {
						t.Fatal(err)
					}
				case "permissions":
					if err := os.WriteFile(f.o.password, []byte("synthetic-password"), 0644); err != nil {
						t.Fatal(err)
					}
				case "oversize":
					if err := os.WriteFile(f.o.password, bytes.Repeat([]byte("x"), 65537), 0600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				f.mode = "wrong-controller"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
				f.o.stop = func(ctx context.Context) error { f.stops++; return ctx.Err() }
			}
			if got := f.run(ctx); got != 1 || f.stops != 1 {
				t.Fatalf("exit=%d stops=%d", got, f.stops)
			}
			if mode == "stop-failure" && !strings.Contains(f.log.String(), "controller_stop_unconfirmed") {
				t.Fatal(f.log.String())
			}
			if strings.Contains(f.log.String(), "private command diagnostic") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestCommandTimeoutTerminatesProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := command(ctx, 5*time.Second, "/bin/sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "watchdog-test", marker)
		done <- err
	}()
	deadline := time.After(3 * time.Second)
	var pid int
	for pid == 0 {
		data, _ := os.ReadFile(marker)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if pid != 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("child did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not terminate")
	}
	deadline = time.After(3 * time.Second)
	for {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) || strings.Contains(string(data), ") Z ") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("descendant survived cancellation")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
