package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testToken = "synthetic-instance-token-never-log"

func input(token string) string {
	raw, _ := json.Marshal(map[string]any{"metadata_url": metadataEndpoint, "callback_url": callbackEndpoint, "callback_token": token, "use_jit": true})
	return string(raw)
}

func TestDecodeRejectsUntrustedConfiguration(t *testing.T) {
	for _, raw := range []string{
		`{`, `{}`, input(""), input("line\nbreak"), input("has space"),
		strings.Replace(input(testToken), metadataEndpoint, metadataEndpoint+"-untrusted", 1),
		strings.Replace(input(testToken), callbackEndpoint, "http://untrusted.invalid", 1),
		strings.Replace(input(testToken), `"use_jit":true`, `"use_jit":false`, 1),
		strings.Replace(input(testToken), `"use_jit":true`, `"unknown":true`, 1),
		input(testToken) + `{}`, strings.Repeat("x", responseLimit+1),
	} {
		if _, err := Decode(strings.NewReader(raw)); err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("configuration rejection was missing or leaked input: %v", err)
		}
	}
	if _, err := Decode(strings.NewReader(input(testToken))); err != nil {
		t.Fatal(err)
	}
}

type callback struct {
	Path string
	Body map[string]any
}

type scenario struct {
	i        *installer
	output   bytes.Buffer
	mu       sync.Mutex
	calls    []callback
	commands [][]string
	fail     string
}

func fixture(t *testing.T, failure string) *scenario {
	t.Helper()
	s := &scenario{fail: failure}
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bin/Runner.Listener", "bin/runsvc.sh", "env.sh"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("image fixture\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	osRelease := filepath.Join(home, "os-release")
	if err := os.WriteFile(osRelease, []byte("NAME=\"Ubuntu\"\nVERSION_ID=\"26.04\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("instance authorization missing")
		}
		var payload map[string]any
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
		}
		s.calls = append(s.calls, callback{r.URL.RequestURI(), payload})
		if failure == "transport" || r.URL.Path == "/api/v1/metadata/credentials/"+failure || failure == "system-info" && strings.HasSuffix(r.URL.Path, "/system-info") || failure == "idle" && payload["status"] == "idle" {
			http.Error(w, testToken, http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/api/v1/metadata/credentials/runner":
			switch failure {
			case "agent-id":
				io.WriteString(w, `{"agentId":1.5}`)
			case "oversize":
				io.WriteString(w, strings.Repeat("x", responseLimit+1))
			case "redirect":
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(http.StatusTemporaryRedirect)
			default:
				io.WriteString(w, `{"agentId":9007199254740993,"agentName":"fixture"}`)
			}
		case "/api/v1/metadata/credentials/credentials", "/api/v1/metadata/credentials/credentials_rsaparams":
			io.WriteString(w, "synthetic-jit-secret")
		case "/api/v1/metadata/system/service-name":
			if failure == "service-name" {
				io.WriteString(w, "../../escape")
			} else {
				io.WriteString(w, "actions.runner.nucleus.fixture\n")
			}
		case "/api/v1/metadata/systemd/unit-file":
			if r.URL.RawQuery != "runAsUser=runner" {
				t.Error("runner user not bound")
			}
			io.WriteString(w, "[Service]\nUser=runner\n")
		case "/api/v1/callbacks/status", "/api/v1/callbacks/system-info":
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := gatewayClient()
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("gateway would use a public proxy")
	}
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "10.203.64.1:9443" {
			return nil, errors.New("untrusted destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	config, err := Decode(strings.NewReader(input(testToken)))
	if err != nil {
		t.Fatal(err)
	}
	s.i = &installer{config: config, home: home, osRelease: osRelease, output: &s.output, client: client}
	s.i.run = func(_ context.Context, args ...string) error {
		if strings.Contains(strings.Join(args, " "), testToken) {
			t.Fatal("token entered command arguments")
		}
		s.commands = append(s.commands, args)
		if failure == "service-start" && len(args) >= 4 && reflect.DeepEqual(args[:4], []string{"sudo", "-n", "systemctl", "start"}) {
			return errors.New("secret command failure " + testToken)
		}
		if failure == "service-install" && len(s.commands) == 1 {
			return errors.New("secret command failure " + testToken)
		}
		return nil
	}
	return s
}

func TestInstallJITIdentityAndReportReady(t *testing.T) {
	s := fixture(t, "")
	if err := os.WriteFile(filepath.Join(s.i.home, "runsvc.sh"), []byte("old image helper"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.i.install(context.Background()); err != nil {
		t.Fatal(err, s.output.String())
	}
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams", ".service", ".nucleiron-bootstrap-ready"} {
		info, err := os.Stat(filepath.Join(s.i.home, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private identity %s: %v %v", name, info, err)
		}
	}
	ready, err := os.ReadFile(filepath.Join(s.i.home, ".nucleiron-bootstrap-ready"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Instance string `json:"instance"`
		AgentID  int64  `json:"agent_id"`
	}
	host, _ := os.Hostname()
	if json.Unmarshal(ready, &receipt) != nil || receipt.Instance != host || receipt.AgentID != 9007199254740993 {
		t.Fatalf("ready receipt changed identity: %s", ready)
	}
	if len(s.commands) != 7 || !reflect.DeepEqual(s.commands[6], []string{"sudo", "-n", "systemctl", "start", "actions.runner.nucleus.fixture.service"}) {
		t.Fatalf("service command order: %#v", s.commands)
	}
	if s.calls[len(s.calls)-1].Body["status"] != "idle" || !strings.Contains(s.output.String(), "bootstrap_complete") {
		t.Fatal("missing successful callback or event")
	}
	if strings.Contains(s.output.String(), testToken) || strings.Contains(s.output.String(), "synthetic-jit-secret") {
		t.Fatal("credential in diagnostics")
	}
	helper, err := os.ReadFile(filepath.Join(s.i.home, "runsvc.sh"))
	if err != nil || string(helper) != "image fixture\n" {
		t.Fatal("runner service helper was not copied from the image")
	}
}

func TestFailuresStopWithoutReadyOrCredentialDiagnostics(t *testing.T) {
	for _, failure := range []string{"transport", "agent-id", "oversize", "redirect", "credentials", "service-name", "service-install", "service-start", "system-info", "idle"} {
		t.Run(failure, func(t *testing.T) {
			s := fixture(t, failure)
			err := s.i.install(context.Background())
			if err == nil {
				t.Fatal("failure accepted")
			}
			if _, err := os.Lstat(filepath.Join(s.i.home, ".nucleiron-bootstrap-ready")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed bootstrap published ready")
			}
			text := s.output.String() + err.Error()
			if !strings.Contains(text, "bootstrap_failed") || strings.Contains(text, testToken) || strings.Contains(text, "synthetic-jit-secret") {
				t.Fatal("unsafe failure diagnostics")
			}
			if failure == "transport" && !strings.Contains(text, "failure_callback_unconfirmed") {
				t.Fatal("lost callback reported as confirmed")
			}
			for _, call := range s.calls {
				if failure != "idle" && call.Body["status"] == "idle" {
					t.Fatal("idle reported before completion")
				}
			}
			if failure == "credentials" {
				if _, err := os.Stat(filepath.Join(s.i.home, ".runner")); err != nil {
					t.Fatal("partial identity was removed")
				}
			}
		})
	}
}

func TestExistingIdentityAndSymlinkArePreserved(t *testing.T) {
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams", ".service", ".nucleiron-bootstrap-ready"} {
		t.Run(name, func(t *testing.T) {
			s := fixture(t, "")
			if err := os.Symlink("missing-original", filepath.Join(s.i.home, name)); err != nil {
				t.Fatal(err)
			}
			if s.i.install(context.Background()) == nil || len(s.commands) != 0 {
				t.Fatal("image containing an identity was reused")
			}
			if dest, err := os.Readlink(filepath.Join(s.i.home, name)); err != nil || dest != "missing-original" {
				t.Fatal("existing identity changed")
			}
		})
	}
}

func TestConcurrentInstallDoesNotChangeActiveStatus(t *testing.T) {
	s := fixture(t, "")
	f, err := os.OpenFile(filepath.Join(s.i.home, ".nucleiron-bootstrap.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if s.i.install(context.Background()) == nil || len(s.calls) != 0 || len(s.commands) != 0 {
		t.Fatal("concurrent installer performed an external operation")
	}
}

func TestCanceledCommandReturnsAfterChildExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- runCommand(ctx, dir, "bash", "-c", `echo ready > started; exec sleep 30`)
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("child stopped before synchronization: %v", err)
		case <-ctx.Done():
			t.Fatal("child did not start")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled command succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped")
	}
}
