package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var serviceName = regexp.MustCompile(`^actions\.runner\.[A-Za-z0-9_.-]+\.service$`)

type installer struct {
	config    Config
	home      string
	osRelease string
	output    io.Writer
	client    *http.Client
	run       func(context.Context, ...string) error
}

// Install configures a fresh guest only. Partial effects are retained for recovery;
// neither a failed command nor a lost callback response authorizes another attempt.
func Install(ctx context.Context, config Config, home, osRelease string, output io.Writer) error {
	client := gatewayClient()
	defer client.Transport.(*http.Transport).CloseIdleConnections()
	i := installer{config: config, home: home, osRelease: osRelease, output: output, client: client}
	i.run = func(ctx context.Context, args ...string) error {
		return runCommand(ctx, home, args...)
	}
	return i.install(ctx)
}

func gatewayClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // The instance TLS gateway must bypass the public proxy.
	return &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // Never forward the instance token to a redirect.
		},
	}
}

func runCommand(ctx context.Context, home string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = home
	// Nil stdio connects to the null device; subprocess diagnostics may contain secrets.
	return cmd.Run()
}

func (i *installer) install(ctx context.Context) error {
	stage := "preflight"
	fail := func(callback bool) error {
		i.event(map[string]any{"event": "bootstrap_failed", "stage": stage})
		if callback {
			// Cancellation ends the install, but failure reporting has its own bounded attempt.
			report, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			if err := i.status(report, "failed", "Bootstrap failed at "+stage, 0); err != nil {
				i.event(map[string]any{"event": "failure_callback_unconfirmed"})
			}
		}
		return errors.New("bootstrap failed at " + stage)
	}
	if i.config.token == "" {
		return fail(false)
	}
	// Keep the inode after release: unlinking a lock could admit a second installer.
	lock, err := os.OpenFile(filepath.Join(i.home, ".nucleiron-bootstrap.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fail(false)
	}
	defer lock.Close()
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return fail(false)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(false) // Do not report another active install as failed.
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	listener, err := os.Stat(filepath.Join(i.home, "bin/Runner.Listener"))
	if err != nil || !listener.Mode().IsRegular() {
		return fail(true)
	}
	for _, name := range []string{".runner", ".credentials", ".credentials_rsaparams", ".service", ".nucleiron-bootstrap-ready"} {
		if _, err := os.Lstat(filepath.Join(i.home, name)); !errors.Is(err, os.ErrNotExist) {
			return fail(true)
		}
	}
	stage = "jit-files"
	if err := i.status(ctx, "installing", "Installing instance JIT identity", 0); err != nil {
		return fail(true)
	}
	var agentID int64
	for _, name := range []string{"runner", "credentials", "credentials_rsaparams"} {
		content, err := i.request(ctx, metadataEndpoint+"/credentials/"+name, nil)
		if err != nil {
			return fail(true)
		}
		if name == "runner" {
			var identity struct {
				AgentID int64 `json:"agentId"`
			}
			if json.Unmarshal(content, &identity) != nil || identity.AgentID <= 0 {
				return fail(true)
			}
			agentID = identity.AgentID
		}
		if err := writeExclusive(filepath.Join(i.home, "."+name), content, 0600); err != nil {
			return fail(true)
		}
	}
	name, err := i.request(ctx, metadataEndpoint+"/system/service-name", nil)
	service := strings.TrimSpace(string(name)) + ".service"
	if err != nil || len(service) > 240 || !serviceName.MatchString(service) {
		return fail(true)
	}
	if err := writeExclusive(filepath.Join(i.home, ".service"), []byte(service+"\n"), 0600); err != nil {
		return fail(true)
	}
	stage = "service-install"
	if err := i.installService(ctx, service); err != nil {
		return fail(true)
	}
	stage = "service-start"
	if err := i.run(ctx, "sudo", "-n", "systemctl", "start", service); err != nil {
		return fail(true)
	}
	osInfo, err := os.ReadFile(i.osRelease)
	if err != nil {
		return fail(true)
	}
	info := map[string]string{}
	for _, line := range strings.Split(string(osInfo), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			info[key] = strings.Trim(value, `"`)
		}
	}
	if _, err := i.request(ctx, callbackEndpoint+"/system-info", map[string]any{"os_name": info["NAME"], "os_version": info["VERSION_ID"], "agent_id": agentID}); err != nil {
		return fail(true)
	}
	if err := i.status(ctx, "idle", "Runner service started", agentID); err != nil {
		return fail(true)
	}
	ready, err := json.Marshal(map[string]any{"instance": i.config.instance, "agent_id": agentID})
	if err != nil || publishReady(i.home, ready) != nil {
		return fail(true)
	}
	i.event(map[string]any{"event": "bootstrap_complete", "agent_id": agentID})
	return nil
}

func (i *installer) installService(ctx context.Context, service string) error {
	unit, err := i.request(ctx, metadataEndpoint+"/systemd/unit-file?runAsUser=runner", nil)
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "nucleiron-unit-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	source := filepath.Join(scratch, service)
	dropin := filepath.Join(scratch, "proxy.conf")
	if err := writeExclusive(source, unit, 0600); err != nil {
		return err
	}
	if err := writeExclusive(dropin, []byte("[Service]\nEnvironmentFile=/etc/nucleiron-proxy.env\n"), 0600); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"sudo", "-n", "install", "-m", "0644", "-o", "root", "-g", "root", source, "/etc/systemd/system/" + service},
		{"sudo", "-n", "install", "-d", "-m", "0755", "/etc/systemd/system/" + service + ".d"},
		{"sudo", "-n", "install", "-m", "0644", "-o", "root", "-g", "root", dropin, "/etc/systemd/system/" + service + ".d/proxy.conf"},
	} {
		if err := i.run(ctx, args...); err != nil {
			return err
		}
	}
	runsvc := filepath.Join(i.home, "bin/runsvc.sh")
	content, err := os.ReadFile(runsvc)
	if err != nil {
		return err
	}
	mode, err := os.Stat(runsvc)
	if err != nil {
		return err
	}
	if err := replaceServiceScript(i.home, content, mode.Mode().Perm()); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"bash", filepath.Join(i.home, "env.sh")},
		{"sudo", "-n", "systemctl", "daemon-reload"},
		{"sudo", "-n", "systemctl", "enable", service},
	} {
		if err := i.run(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func (i *installer) request(ctx context.Context, url string, payload any) ([]byte, error) {
	var body io.Reader
	method := http.MethodGet
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body, method = bytes.NewReader(raw), http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+i.config.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := i.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("bootstrap request rejected")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil || len(content) > responseLimit {
		return nil, errors.New("invalid bootstrap response")
	}
	return content, nil
}

func (i *installer) status(ctx context.Context, state, message string, agentID int64) error {
	payload := map[string]any{"status": state, "message": message}
	if agentID > 0 {
		payload["agent_id"] = agentID
	}
	_, err := i.request(ctx, callbackEndpoint+"/status", payload)
	return err
}

func (i *installer) event(payload map[string]any) {
	// Only fixed events, stage names, and the validated agent ID reach diagnostics.
	_ = json.NewEncoder(i.output).Encode(payload)
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	return errors.Join(writeErr, syncErr, f.Close())
}

func publishReady(home string, data []byte) error {
	f, err := os.CreateTemp(home, ".bootstrap-ready-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	if err := errors.Join(writeErr, syncErr, f.Close()); err != nil {
		return err
	}
	// Link publishes only the complete, synced receipt and refuses an existing name.
	if err := os.Link(f.Name(), filepath.Join(home, ".nucleiron-bootstrap-ready")); err != nil {
		return err
	}
	dir, err := os.Open(home)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func replaceServiceScript(home string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(home, ".runsvc-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	chmodErr := f.Chmod(mode)
	syncErr := f.Sync()
	if err := errors.Join(writeErr, chmodErr, syncErr, f.Close()); err != nil {
		return err
	}
	// runsvc.sh is an image helper, not an instance identity; replace it without
	// following a preexisting link, matching the original copy's intended result.
	return os.Rename(f.Name(), filepath.Join(home, "runsvc.sh"))
}

// ReportInvalidInput avoids printing decoder errors that may contain credentials.
func ReportInvalidInput(output io.Writer) {
	fmt.Fprintln(output, `{"event":"bootstrap_failed","stage":"preflight"}`)
}
