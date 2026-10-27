// Package watchdog stops admission after a failed provider health check.
package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

var errRefused = errors.New("watchdog operation not confirmed")
var errBusy = errors.New("watchdog already running")
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var scaleName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

type binding struct {
	Controller string `json:"controller_id"`
	Repository string `json:"repository_id"`
	ScaleSet   string `json:"scaleset_name"`
}

func (b binding) valid() bool {
	return uuid.MatchString(b.Controller) && uuid.MatchString(b.Repository) && scaleName.MatchString(b.ScaleSet)
}

type receipt struct {
	Version        int     `json:"version"`
	Binding        binding `json:"binding"`
	LoginPending   bool    `json:"login_pending"`
	DisablePending int64   `json:"disable_pending"`
}

type observer struct {
	dir, policy, password string
	uid                   uint32
	output                io.Writer
	check                 func(context.Context) (int, error)
	stop                  func(context.Context) error
	api                   *controller
}

// Run uses only fixed host endpoints. Tests inject local adapters inside this package.
func Run(ctx context.Context, output io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(output, `{"event":"watchdog_failed","stage":"root_identity"}`)
		return 1
	}
	api := newController()
	defer api.client.CloseIdleConnections()
	o := observer{
		dir: "/var/lib/nucleiron-watchdog", policy: "/etc/nucleus-halo/provider-policy.json",
		password: "/etc/nucleus-halo/admin-password", output: output, api: api,
		check: func(ctx context.Context) (int, error) {
			return command(ctx, 90*time.Second, "/usr/sbin/runuser", "-u", "nucleiron", "-g", "nucleiron", "-G", "lxd", "--", "/opt/nucleus-halo/bin/guarded-provider", "--watchdog-check")
		},
		stop: func(ctx context.Context) error {
			code, err := command(ctx, 15*time.Second, "/usr/bin/systemctl", "stop", "--no-block", "nucleiron.service")
			if err != nil || code != 0 {
				return errRefused
			}
			return nil
		},
	}
	return o.run(ctx)
}

func command(ctx context.Context, timeout time.Duration, name string, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run() // Null stdio keeps credentials and subprocess errors out of the journal.
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (o *observer) run(parent context.Context) int {
	lock, err := o.lock()
	if errors.Is(err, errBusy) {
		return 75
	}
	if err != nil {
		o.event("state_directory", "inspect_state")
		return 1
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// A previous stop request owns its original systemd job, even after a lost reply.
	if _, err := os.Lstat(filepath.Join(o.dir, "stop-request.json")); !errors.Is(err, os.ErrNotExist) {
		o.event("previous_stop", "inspect_original_stop")
		return 1
	}
	ctx, cancel := context.WithTimeout(parent, 125*time.Second)
	defer cancel()
	if stage, err := o.observe(ctx); err != nil {
		return o.fallback(parent, stage)
	}
	return 0
}

func (o *observer) observe(ctx context.Context) (string, error) {
	state := receipt{Version: 1}
	raw, err := readProtected(filepath.Join(o.dir, "state.json"), o.uid, 0600, 8192)
	if err == nil {
		if json.Unmarshal(raw, &state) != nil || state.Version != 1 || !state.Binding.valid() || state.DisablePending < 0 || state.DisablePending > 4294967295 {
			return "state", errRefused
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "state", err
	}
	if state.LoginPending {
		return "login_unknown", errRefused
	}
	code, checkErr := o.check(ctx)
	if checkErr == nil && (code == 0 || code == 75) && state.DisablePending == 0 {
		return "", nil
	}
	raw, err = readProtected(o.policy, o.uid, 0644, 65536)
	var target binding
	// The shared policy also contains fields owned by guard. Only these three
	// fields belong to this consumer; missing or invalid identities are rejected.
	if err != nil || json.Unmarshal(raw, &target) != nil || !target.valid() {
		return "policy", errRefused
	}
	if state.Binding != (binding{}) && state.Binding != target {
		return "policy_changed", errRefused
	}
	state.Binding = target
	secret, err := readProtected(o.password, o.uid, 0600, 65536)
	if err != nil {
		return "credentials", err
	}
	state.LoginPending = true
	if err := o.save(state); err != nil {
		return "login_intent", err
	}
	token, err := o.api.login(ctx, secret)
	if err != nil {
		return "login_unknown", err
	}
	state.LoginPending = false
	if err := o.save(state); err != nil {
		return "login_receipt", err
	}
	if err := o.api.identity(ctx, token, target.Controller); err != nil {
		return "controller_identity", err
	}
	result, err := o.disable(ctx, token, &state)
	if err != nil {
		return "disable_unconfirmed", err
	}
	_ = json.NewEncoder(o.output).Encode(map[string]string{"event": "runner_admission_stopped", "result": result})
	return "", nil
}

func (o *observer) disable(ctx context.Context, token string, state *receipt) (string, error) {
	if state.DisablePending != 0 {
		// Recover only by reading the exact original target. Seeing it enabled or
		// absent does not prove that the first request failed or authorize a retry.
		current, err := o.api.scaleSet(ctx, token, state.DisablePending)
		if err != nil || !current.matches(state.Binding, state.DisablePending) || bool(current.Enabled) {
			return "", errRefused
		}
		state.DisablePending = 0
		return "disabled", o.save(*state)
	}
	items, err := o.api.scaleSets(ctx, token, state.Binding.Repository)
	if err != nil {
		return "", err
	}
	var selected *scaleSet
	for _, item := range items {
		if item.Name != state.Binding.ScaleSet {
			continue
		}
		if selected != nil || !item.matches(state.Binding, item.ID) {
			return "", errRefused
		}
		copy := item
		selected = &copy
	}
	if selected == nil {
		return "no-scaleset", nil
	}
	current, err := o.api.scaleSet(ctx, token, selected.ID)
	if err != nil || !current.matches(state.Binding, selected.ID) {
		return "", errRefused
	}
	if !current.Enabled {
		return "disabled", nil
	}
	state.DisablePending = selected.ID
	if err := o.save(*state); err != nil {
		return "", err
	}
	if err := o.api.disable(ctx, token, selected.ID); err != nil {
		return "", err
	}
	current, err = o.api.scaleSet(ctx, token, selected.ID)
	if err != nil || !current.matches(state.Binding, selected.ID) || bool(current.Enabled) {
		return "", errRefused
	}
	state.DisablePending = 0
	return "disabled", o.save(*state)
}

func (o *observer) fallback(parent context.Context, stage string) int {
	// Persist before requesting a stop; no subsequent tick repeats an unknown job.
	path := filepath.Join(o.dir, "stop-request.json")
	if err := writeExclusive(path, []byte(`{"version":1,"status":"pending"}`)); err != nil {
		o.event(stage, "stop_not_sent_inspect_state")
		return 1
	}
	if err := syncDir(o.dir); err != nil {
		o.event(stage, "stop_not_sent_inspect_state")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer cancel()
	if err := o.stop(ctx); err != nil {
		o.event(stage, "controller_stop_unconfirmed")
		return 1
	}
	// systemctl --no-block confirms submission, not service or job termination.
	if err := atomicJSON(o.dir, "stop-request.json", map[string]any{"version": 1, "status": "requested"}); err != nil {
		o.event(stage, "controller_stop_unconfirmed")
		return 1
	}
	o.event(stage, "controller_stop_requested_inspect_state")
	return 1
}

func (o *observer) event(stage, action string) {
	_ = json.NewEncoder(o.output).Encode(map[string]string{"event": "watchdog_failed", "stage": stage, "action": action})
}

func (o *observer) save(state receipt) error { return atomicJSON(o.dir, "state.json", state) }
