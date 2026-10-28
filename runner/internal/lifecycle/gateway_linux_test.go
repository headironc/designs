package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"github.com/headironc/designs/runner/internal/provider"
	"golang.org/x/sys/unix"
)

type gatewayFixtureManager struct {
	*pauseTestManager
	mu                         sync.Mutex
	host                       gatewayHost
	parent, mode               string
	stopped, controllerGone    bool
	sends, controllerReads     int
	gatewayReads               int
	eventEntered, drainEntered chan struct{}
}

func (f *gatewayFixtureManager) Subscribe() *dbus.Error { return f.pauseTestManager.Subscribe() }
func (f *gatewayFixtureManager) ListJobs() ([]systemd.JobStatus, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped && f.mode == "late_job" {
		return []systemd.JobStatus{{Id: 99, Unit: gatewayUnit, JobType: "start", Status: "waiting", JobPath: dbus.ObjectPath(pauseJobPath(99)), UnitPath: "/org/freedesktop/systemd1/unit/gateway"}}, nil
	}
	if f.mode == "second_job" && f.gatewayReads >= 2 {
		return []systemd.JobStatus{{Id: 99, Unit: gatewayUnit, JobType: "start", Status: "waiting", JobPath: dbus.ObjectPath(pauseJobPath(99)), UnitPath: "/org/freedesktop/systemd1/unit/gateway"}}, nil
	}
	return []systemd.JobStatus{}, nil
}

func heldFixtureLock(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	return errors.Is(unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK)
}

func (f *gatewayFixtureManager) StopUnit(unit, mode string) (dbus.ObjectPath, *dbus.Error) {
	if unit == pauseTimer {
		return f.pauseTestManager.StopUnit(unit, mode)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if unit != gatewayUnit || mode != "fail" || !heldFixtureLock(filepath.Join(f.parent, "pause.lock")) || !heldFixtureLock(filepath.Join(f.parent, "operation.lock")) {
		return "", dbus.MakeFailedError(errors.New("wrong target or missing original locks"))
	}
	if _, err := os.Stat(filepath.Join(f.parent, "gateway-stop/intent.json")); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	f.sends++
	f.stopped = true
	if f.mode != "hold" {
		if err := os.RemoveAll(filepath.Join(f.host.proc, "9001")); err != nil {
			return "", dbus.MakeFailedError(err)
		}
		if err := os.WriteFile(filepath.Join(f.host.cgroup, gatewayGroup, "cgroup.events"), []byte("populated 0\n"), 0600); err != nil {
			return "", dbus.MakeFailedError(err)
		}
	}
	if f.mode == "controller_after_stopped" {
		f.controllerGone = true
	}
	result, signalUnit := "done", gatewayUnit
	if f.mode == "failed_job" {
		result = "failed"
	}
	if f.mode == "wrong_event" {
		signalUnit = controllerUnit
	}
	path := dbus.ObjectPath(pauseJobPath(43))
	if f.mode != "missing_event" {
		if err := f.conn.Emit(pauseManagerPath, pauseManager+".Manager.JobRemoved", uint32(43), path, signalUnit, result); err != nil {
			return "", dbus.MakeFailedError(err)
		}
	}
	if f.mode == "lost_reply" {
		return "", dbus.MakeFailedError(errors.New("fixture-sensitive-never-report"))
	}
	return path, nil
}

type gatewayFixtureProperties struct {
	f    *gatewayFixtureManager
	unit string
}

func (p *gatewayFixtureProperties) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	base, err := (&pauseTestProperties{p.f.pauseTestManager, pauseWorker}).GetAll(iface)
	if err != nil {
		return nil, err
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	f := p.f
	raw := map[string]any{}
	for k, v := range base {
		raw[k] = v.Value()
	}
	if iface == pauseManager+".Unit" {
		raw["Id"], raw["FragmentPath"] = p.unit, "/etc/systemd/system/"+p.unit
		raw["Requires"], raw["Wants"], raw["Requisite"], raw["RequiredBy"], raw["RequisiteOf"], raw["Triggers"], raw["TriggeredBy"] = []string{}, []string{}, []string{}, []string{}, []string{}, []string{}, []string{}
		raw["SuccessAction"], raw["FailureAction"], raw["JobTimeoutAction"], raw["StartLimitAction"] = "none", "none", "none", "none"
		raw["ActiveState"], raw["SubState"] = "active", "running"
		if p.unit == gatewayUnit {
			f.gatewayReads++
			raw["InvocationID"] = []byte("gateway-invoc-01")
			raw["Requires"] = []string{controllerUnit, firewallUnit}
			if f.stopped {
				raw["ActiveState"], raw["SubState"] = "inactive", "dead"
			}
			switch f.mode {
			case "failure_action":
				raw["FailureAction"] = "reboot"
			case "success_action":
				raw["SuccessAction"] = "poweroff"
			case "job_timeout_action":
				raw["JobTimeoutAction"] = "reboot"
			case "missing_action":
				delete(raw, "SuccessAction")
			case "wrong_action":
				raw["FailureAction"] = true
			case "reverse_stop":
				raw["RequiredBy"] = []string{controllerUnit}
			case "hook":
				raw["OnSuccess"] = []string{"other.service"}
			case "wants_unused":
				raw["Wants"] = []string{"aux.service"}
			case "requisite_unused":
				raw["Requisite"] = []string{"aux.service"}
			case "late_job":
				if f.stopped {
					raw["Job"] = pauseTestJob{99, dbus.ObjectPath(pauseJobPath(99))}
				}
			case "second_job":
				if f.gatewayReads >= 2 {
					raw["Job"] = pauseTestJob{99, dbus.ObjectPath(pauseJobPath(99))}
				}
			case "second_reactivation":
				if f.gatewayReads >= 2 {
					raw["ActiveState"], raw["SubState"] = "active", "running"
				}
			case "replacement":
				if f.stopped {
					raw["InvocationID"] = []byte("gateway-invoc-02")
				}
			case "reactivated":
				if f.stopped {
					raw["ActiveState"], raw["SubState"] = "active", "running"
				}
			}
		} else if p.unit == controllerUnit {
			f.controllerReads++
			raw["InvocationID"] = []byte("controller-00001")
			if f.controllerGone {
				raw["ActiveState"], raw["SubState"] = "inactive", "dead"
			}
			if f.mode == "controller_unused" {
				raw["StopWhenUnneeded"] = true
			}
		} else if p.unit == firewallUnit {
			raw["SubState"] = "exited"
			if f.mode == "firewall_unused" {
				raw["StopWhenUnneeded"] = true
			}
		} else if p.unit == "aux.service" {
			raw["StopWhenUnneeded"] = true
		}
	} else if iface == pauseManager+".Service" {
		if p.unit == gatewayUnit {
			raw["Type"], raw["Restart"], raw["User"], raw["Group"], raw["DynamicUser"] = "simple", "on-failure", "nucleiron-gateway", "", true
			raw["KillMode"], raw["KillSignal"], raw["FinalKillSignal"], raw["SendSIGKILL"], raw["SendSIGHUP"], raw["TimeoutStopUSec"], raw["TimeoutStopFailureMode"] = "control-group", int32(15), int32(9), true, false, uint64(90000000), "terminate"
			raw["MainPID"], raw["ControlPID"], raw["ExecMainStartTimestampMonotonic"], raw["ControlGroup"] = uint32(9001), uint32(0), uint64(555), gatewayGroup
			raw["ExecStart"] = []pauseTestExec{{Path: "/usr/sbin/nginx", Args: []string{"/usr/sbin/nginx", "-c", "/etc/nucleiron-gateway/gateway.conf"}}}
			raw["ExecStartPre"] = []pauseTestExec{{Path: "/usr/sbin/nginx", Args: []string{"/usr/sbin/nginx", "-t", "-c", "/etc/nucleiron-gateway/gateway.conf"}}}
			raw["ExecReload"] = []pauseTestExec{}
			if f.stopped {
				raw["MainPID"], raw["ControlGroup"] = uint32(0), ""
			}
			switch f.mode {
			case "kill_mode":
				raw["KillMode"] = "process"
			case "kill_type":
				raw["KillSignal"] = uint32(15)
			case "dynamic_missing":
				delete(raw, "DynamicUser")
			case "success_exit":
				raw["SuccessExitStatus"] = pauseTestExitStatus{[]int32{1}, []int32{}}
			case "success_signal":
				raw["SuccessExitStatus"] = pauseTestExitStatus{[]int32{}, []int32{15}}
			case "exec_order":
				raw["ExecStart"] = []pauseTestExec{{Path: "/usr/sbin/nginx", Args: []string{"-c", "/usr/sbin/nginx", "/etc/nucleiron-gateway/gateway.conf"}}}
			}
		} else if p.unit == controllerUnit {
			raw["Type"], raw["User"], raw["Group"], raw["DynamicUser"] = "simple", "nucleiron", "nucleiron", false
			raw["MainPID"], raw["ControlPID"], raw["ExecMainStartTimestampMonotonic"], raw["ControlGroup"] = uint32(9002), uint32(0), uint64(777), "/system.slice/"+controllerUnit
			raw["ExecStart"] = []pauseTestExec{{Path: "/opt/nucleus-halo/bin/garm", Args: []string{"/opt/nucleus-halo/bin/garm", "-config", controllerConfig}}}
			if f.controllerGone {
				raw["MainPID"] = uint32(0)
			}
		}
	}
	out := map[string]dbus.Variant{}
	for k, v := range raw {
		out[k] = dbus.MakeVariant(v)
	}
	return out, nil
}

type gatewayTestConnection struct {
	gatewayConnection
	fixture *gatewayFixtureManager
}

func (b gatewayTestConnection) event(ctx context.Context, j pauseJob) (pauseEvent, error) {
	select {
	case b.fixture.eventEntered <- struct{}{}:
	default:
	}
	return b.gatewayConnection.event(ctx, j)
}
func (b gatewayTestConnection) drained(ctx context.Context, a, c gatewayFacts) error {
	err := b.gatewayConnection.drained(ctx, a, c)
	if errors.Is(err, errPauseBusy) {
		select {
		case b.fixture.drainEntered <- struct{}{}:
		default:
		}
	}
	return err
}

func gatewayProc(t *testing.T, h gatewayHost, pid int, state, start, unit string, uid, gid uint32, exe string) {
	t.Helper()
	dir := filepath.Join(h.proc, fmt.Sprint(pid))
	testWrite(t, filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (service (fixture)) %s 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 %s\n", pid, state, start)), 0444)
	testWrite(t, filepath.Join(dir, "status"), []byte(fmt.Sprintf("Uid:\t%d %d %d %d\nGid:\t%d %d %d %d\n", uid, uid, uid, uid, gid, gid, gid, gid)), 0444)
	testWrite(t, filepath.Join(dir, "cgroup"), []byte("0::/system.slice/"+unit+"\n"), 0444)
	testWrite(t, filepath.Join(dir, "cmdline"), []byte("nginx: master process rewritten title\x00"), 0444)
	if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
}

func gatewayFixture(t *testing.T) (gatewayStopper, *gatewayFixtureManager, receipt, string) {
	t.Helper()
	p, _, r, sha := pauseFixture(t)
	f, socket, ph := privatePauseBus(t)
	h := gatewayHost{proc: ph.proc, cgroup: ph.cgroup, nginx: filepath.Join(filepath.Dir(ph.proc), "nginx"), controllerExe: filepath.Join(filepath.Dir(ph.proc), "garm")}
	testWrite(t, h.nginx, []byte("independent synthetic nginx image"), 0755)
	testWrite(t, h.controllerExe, []byte("independent synthetic controller image"), 0755)
	gatewayProc(t, h, 9001, "S", "1234", gatewayUnit, 24567, 24568, h.nginx)
	gatewayProc(t, h, 9002, "S", "3456", controllerUnit, 24600, 24601, h.controllerExe)
	testWrite(t, filepath.Join(h.cgroup, gatewayGroup, "cgroup.events"), []byte("populated 1\n"), 0600)
	m := &gatewayFixtureManager{pauseTestManager: f, host: h, parent: p.parent, eventEntered: make(chan struct{}, 1), drainEntered: make(chan struct{}, 1)}
	if err := f.conn.Export(m, pauseManagerPath, pauseManager+".Manager"); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{gatewayUnit, controllerUnit, firewallUnit, "aux.service"} {
		path := "/org/freedesktop/systemd1/unit/" + strings.ReplaceAll(strings.ReplaceAll(unit, "-", "_2d"), ".", "_2e")
		if err := f.conn.Export(&gatewayFixtureProperties{m, unit}, dbus.ObjectPath(path), "org.freedesktop.DBus.Properties"); err != nil {
			t.Fatal(err)
		}
	}
	p.connect = func(c context.Context) (pauseConnection, error) { return connectPause(c, socket, ph) }
	p.boot = ph.boot
	if _, err := p.execute(context.Background(), r, sha); err != nil {
		t.Fatal(err)
	}
	originalPause := map[string]string{}
	for _, name := range []string{"intent.json", "job.json", "event.json", "drain.json"} {
		raw, e := os.ReadFile(filepath.Join(p.parent, "pause", name))
		if e != nil {
			t.Fatal(e)
		}
		originalPause[name] = string(raw)
	}
	t.Cleanup(func() {
		entries, e := os.ReadDir(filepath.Join(p.parent, "pause"))
		if e != nil || len(entries) != 4 {
			t.Error("original pause file set changed")
		}
		for name, want := range originalPause {
			raw, e := os.ReadFile(filepath.Join(p.parent, "pause", name))
			if e != nil || string(raw) != want {
				t.Error("original pause bytes changed", name)
			}
		}
	})
	testWrite(t, filepath.Join(p.parent, "operation.lock"), nil, 0600)
	g := gatewayStopper{pause: p, producer: strings.Repeat("9", 64), admission: func(context.Context, receipt) (string, error) { return strings.Repeat("8", 64), nil }, interval: time.Millisecond, connect: func(c context.Context) (gatewayConnection, error) {
		b, e := connectGateway(c, socket, h)
		return gatewayTestConnection{b, m}, e
	}}
	return g, m, r, sha
}

func TestGatewayOriginalResultAndControllerLaterStop(t *testing.T) {
	g, f, r, sha := gatewayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := g.execute(ctx, r, sha)
	if err != nil || out.State != "gateway_stopped_drained" || out.Job != 43 || out.JobResult != "done" {
		t.Fatalf("%+v %v", out, err)
	}
	raw, e := os.ReadFile(filepath.Join(g.pause.parent, "gateway-stop/intent.json"))
	if e != nil {
		t.Fatal(e)
	}
	var original struct {
		Unit      string `json:"unit"`
		Operation string `json:"operation_id"`
		Before    struct {
			Main struct {
				PID   uint32 `json:"pid"`
				Start uint64 `json:"starttime_ticks"`
				UID   uint32 `json:"uid"`
				GID   uint32 `json:"gid"`
			}
		} `json:"before"`
		Controller struct {
			Main struct {
				PID uint32 `json:"pid"`
			}
		} `json:"controller"`
	}
	if json.Unmarshal(raw, &original) != nil || original.Unit != "nucleiron-gateway.service" || original.Operation != "aaaaaaaa-0000-4000-8000-000000000001" || original.Before.Main.PID != 9001 || original.Before.Main.Start != 1234 || original.Before.Main.UID != 24567 || original.Before.Main.GID != 24568 || original.Controller.Main.PID != 9002 {
		t.Fatalf("independent original identity: %+v", original)
	}
	f.mu.Lock()
	f.controllerGone = true
	f.controllerReads = 0
	f.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(f.host.proc, "9002")); err != nil {
		t.Fatal(err)
	}
	g.admission = func(context.Context, receipt) (string, error) {
		t.Fatal("recovery requested fresh credentials")
		return "", errRefused
	}
	if _, err := g.execute(ctx, r, sha); err != nil {
		t.Fatal(err)
	}
	if got := g.observe(ctx, r, sha); got.State != "gateway_stopped_drained" || got.JobResult != "done" {
		t.Fatalf("later controller stop invalidated original: %+v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 1 || f.controllerReads != 0 {
		t.Fatalf("recovery resent or queried stopped controller: %d %d", f.sends, f.controllerReads)
	}
}

func TestGatewayUnsafeLoadedConfigurationsRefuseBeforeSend(t *testing.T) {
	for _, mode := range []string{"failure_action", "success_action", "job_timeout_action", "missing_action", "wrong_action", "reverse_stop", "hook", "wants_unused", "requisite_unused", "controller_unused", "firewall_unused", "kill_mode", "kill_type", "dynamic_missing", "exec_order", "success_exit", "success_signal"} {
		t.Run(mode, func(t *testing.T) {
			g, f, r, sha := gatewayFixture(t)
			f.mu.Lock()
			f.mode = mode
			f.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := g.execute(ctx, r, sha); err == nil {
				t.Fatal("unsafe loaded configuration admitted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 0 {
				t.Fatal("unsafe first send")
			}
		})
	}
}

func drainGatewaySignal(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
func gatewayWaitAtEvent(t *testing.T, g gatewayStopper, f *gatewayFixtureManager, r receipt, sha string) (gatewayOutcome, error) {
	t.Helper()
	drainGatewaySignal(f.eventEntered)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		o gatewayOutcome
		e error
	}
	done := make(chan result, 1)
	go func() { o, e := g.execute(ctx, r, sha); done <- result{o, e} }()
	select {
	case <-f.eventEntered:
		cancel()
	case <-ctx.Done():
		t.Fatal("original event wait not reached")
	}
	got := <-done
	return got.o, got.e
}

func TestGatewayUnknownResultsNeverResend(t *testing.T) {
	for _, mode := range []string{"lost_reply", "missing_event", "failed_job", "wrong_event", "replacement", "reactivated", "late_job", "controller_after_stopped"} {
		t.Run(mode, func(t *testing.T) {
			g, f, r, sha := gatewayFixture(t)
			f.mu.Lock()
			f.mode = mode
			f.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var out gatewayOutcome
			var err error
			if mode == "missing_event" {
				out, err = gatewayWaitAtEvent(t, g, f, r, sha)
			} else {
				out, err = g.execute(ctx, r, sha)
			}
			if err == nil || out.State == "gateway_stopped_drained" {
				t.Fatal("unknown/failed stop completed")
			}
			if mode == "failed_job" && (out.State != "gateway_job_failed" || out.JobResult != "failed") {
				t.Fatalf("lost failed job %+v", out)
			}
			if mode == "controller_after_stopped" && (out.Code != "controller_window_unknown" || out.JobResult != "done") {
				t.Fatalf("lost original event/window gap %+v", out)
			}
			g.admission = func(context.Context, receipt) (string, error) {
				t.Fatal("recovery requested credentials")
				return "", errRefused
			}
			if mode == "missing_event" || mode == "wrong_event" {
				gatewayWaitAtEvent(t, g, f, r, sha)
			} else {
				g.execute(ctx, r, sha)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 1 {
				t.Fatalf("original stop resent %d", f.sends)
			}
		})
	}
}

func TestGatewayNaturalDrainCancellationAndOriginalRecovery(t *testing.T) {
	g, f, r, sha := gatewayFixture(t)
	f.mu.Lock()
	f.mode = "hold"
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := g.execute(ctx, r, sha); done <- e }()
	select {
	case <-f.drainEntered:
		cancel()
	case <-ctx.Done():
		t.Fatal("original process drain not observed")
	}
	if err := <-done; err == nil {
		t.Fatal("live original PID completed")
	}
	if err := os.RemoveAll(filepath.Join(f.host.proc, "9001")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.host.cgroup, gatewayGroup, "cgroup.events"), []byte("populated 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	g.admission = func(context.Context, receipt) (string, error) {
		t.Fatal("new credentials on recovery")
		return "", errRefused
	}
	if out, err := g.execute(context.Background(), r, sha); err != nil || out.State != "gateway_stopped_drained" {
		t.Fatalf("original recovery %+v %v", out, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 1 {
		t.Fatal("drain recovery resent")
	}
}

func TestGatewayPersistenceAndControllerWindowRecovery(t *testing.T) {
	for _, name := range []string{"intent.json", "job.json", "event.json", "drain.json"} {
		t.Run(name, func(t *testing.T) {
			g, f, r, sha := gatewayFixture(t)
			failed := false
			g.pause.files.syncHook = func(file *os.File) error {
				if file.Name() == name && !failed {
					failed = true
					return errRefused
				}
				return file.Sync()
			}
			if _, err := g.execute(context.Background(), r, sha); err == nil || !failed {
				t.Fatal("sync failure not retained")
			}
			g.pause.files.syncHook = nil
			var err error
			if name == "job.json" {
				// The early event belonged to the closed first connection. A saved
				// job does not make that unrecorded event durable.
				var out gatewayOutcome
				out, err = gatewayWaitAtEvent(t, g, f, r, sha)
				if out.Job != 43 || out.JobResult != "" {
					t.Fatal("original job was not read back", out)
				}
			} else {
				_, err = g.execute(context.Background(), r, sha)
			}
			if (err == nil) != (name == "event.json" || name == "drain.json") {
				t.Fatalf("original durability recovery: %v", err)
			}
			want := 1
			if name == "intent.json" {
				want = 0
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != want {
				t.Fatalf("sync recovery resent: %d", f.sends)
			}
		})
	}
	for _, controllerStopped := range []bool{false, true} {
		t.Run(fmt.Sprint(controllerStopped), func(t *testing.T) {
			g, f, r, sha := gatewayFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			interrupted := false
			g.pause.files.syncHook = func(file *os.File) error {
				e := file.Sync()
				if e == nil && file.Name() == "event.json" && !interrupted {
					interrupted = true
					cancel()
				}
				return e
			}
			out, err := g.execute(ctx, r, sha)
			if err == nil || out.JobResult != "done" || !interrupted {
				t.Fatalf("event boundary %+v %v", out, err)
			}
			g.pause.files.syncHook = nil
			f.mu.Lock()
			f.controllerGone = controllerStopped
			f.mu.Unlock()
			out, err = g.execute(context.Background(), r, sha)
			if controllerStopped {
				if err == nil || out.Code != "controller_window_unknown" || out.JobResult != "done" {
					t.Fatalf("missing original window fabricated %+v %v", out, err)
				}
			} else if err != nil || out.State != "gateway_stopped_drained" {
				t.Fatalf("original controller window not recovered %+v %v", out, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 1 {
				t.Fatal("window recovery resent")
			}
		})
	}
}

func TestGatewayExistingStopAndOriginalLocksBlockNewRequests(t *testing.T) {
	for _, mode := range []string{"stop_file", "stop_directory", "stop_symlink", "operation_missing", "operation_busy", "pause_busy"} {
		t.Run(mode, func(t *testing.T) {
			g, f, r, sha := gatewayFixture(t)
			var lock *os.File
			switch mode {
			case "stop_file":
				testWrite(t, filepath.Join(g.pause.parent, "stop-request.json"), []byte(`{"job":0}`), 0600)
			case "stop_directory":
				if e := os.Mkdir(filepath.Join(g.pause.parent, "stop-request.json"), 0700); e != nil {
					t.Fatal(e)
				}
			case "stop_symlink":
				if e := os.Symlink("missing", filepath.Join(g.pause.parent, "stop-request.json")); e != nil {
					t.Fatal(e)
				}
			case "operation_missing":
				if e := os.Remove(filepath.Join(g.pause.parent, "operation.lock")); e != nil {
					t.Fatal(e)
				}
			case "operation_busy", "pause_busy":
				name := "operation.lock"
				if mode == "pause_busy" {
					name = "pause.lock"
				}
				var e error
				lock, e = os.OpenFile(filepath.Join(g.pause.parent, name), os.O_RDONLY, 0)
				if e != nil || unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
					t.Fatal("fixture lock", e)
				}
				defer lock.Close()
			}
			if _, err := g.execute(context.Background(), r, sha); err == nil {
				t.Fatal("occupied original owner bypassed")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 0 {
				t.Fatal("stop sent without original locks")
			}
		})
	}
}

func TestGatewayDiagnosticKeepsClosureUnknown(t *testing.T) {
	g, f, r, sha := gatewayFixture(t)
	if _, err := g.execute(context.Background(), r, sha); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.controllerGone = true
	f.mu.Unlock()
	before := map[string]string{}
	for _, name := range []string{"intent.json", "job.json", "event.json", "drain.json"} {
		raw, e := os.ReadFile(filepath.Join(g.pause.parent, "gateway-stop", name))
		if e != nil {
			t.Fatal(e)
		}
		before[name] = string(raw)
	}
	d := diagnostician{job: func(context.Context, receipt) jobObservation {
		return jobObservation{State: "terminal", Conclusion: "failure"}
	}, registration: func(context.Context, receipt) resourceObservation { return resource("absent_verified", 0) }, garm: func(context.Context, receipt) garmObservation {
		return garmObservation{resourceObservation: resource("absent_verified", 0)}
	}, machine: func(context.Context, receipt) machineObservation { return machineObservation{} }, host: func(context.Context, receipt) hostObservation { return hostObservation{} }, guard: func(context.Context) (provider.GuardObservation, error) {
		return provider.GuardObservation{}, errRefused
	}, services: func(context.Context) []serviceObservation { return []serviceObservation{} }, database: func(context.Context, receipt) databaseObservation { return databaseObservation{} }, gateway: func(ctx context.Context) gatewayOutcome { return g.observe(ctx, r, sha) }}
	report := d.observe(context.Background(), r, strings.Repeat("a", 64))
	if report.Closure != "unknown" || report.GatewayStop.State != "gateway_stopped_drained" || report.Job.Conclusion != "failure" || !reflect.DeepEqual(report.ProvenanceGaps, []string{"original_service_jobs_missing", "execution_credentials_closure_missing", "external_operation_receipts_missing", "original_controller_configuration_provenance_missing"}) {
		t.Fatalf("narrow gateway result upgraded closure %+v", report)
	}
	for name, want := range before {
		raw, e := os.ReadFile(filepath.Join(g.pause.parent, "gateway-stop", name))
		if e != nil || string(raw) != want {
			t.Fatal("diagnostic rewrote original", name)
		}
	}
}

func TestGatewayEntrypointRejectsAlternateStateAndRedacts(t *testing.T) {
	var out, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"stop-gateway", "--gateway-state", "/fixture-sensitive"}, &out, &diagnostic)
	if code != 1 || out.Len() != 0 || diagnostic.String() != "{\"event\":\"gateway_stop_refused\",\"stage\":\"arguments\"}\n" {
		t.Fatalf("%d %q %q", code, out.String(), diagnostic.String())
	}
}

func TestGatewayUsesCompleteLiveSDKAdmission(t *testing.T) {
	g, _, r, sha := gatewayFixture(t)
	bodies := diagnosticBodies()
	bodies["/repos/example/consumer/actions/jobs/9007199254740997"] = strings.Replace(bodies["/repos/example/consumer/actions/jobs/9007199254740997"], `"status":"in_progress"`, `"status":"completed","conclusion":"cancelled"`, 1)
	bodies["/api/v1/scalesets"] = "[" + bodies["/api/v1/scalesets/27"] + "]"
	bodies["/api/v1/pools"] = "[]"
	g.admission = platformFixture(t, bodies).pauseAdmission
	if out, err := g.execute(context.Background(), r, sha); err != nil || out.State != "gateway_stopped_drained" {
		t.Fatalf("live SDK admission: %+v %v", out, err)
	}
}

func TestGatewayIntentPreventsSendBoundaryRetry(t *testing.T) {
	for _, mode := range []string{"admission_changed", "old_stop_appeared", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			g, f, r, sha := gatewayFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			calls := 0
			g.admission = func(context.Context, receipt) (string, error) {
				calls++
				if calls == 3 {
					switch mode {
					case "admission_changed":
						return strings.Repeat("7", 64), nil
					case "old_stop_appeared":
						testWrite(t, filepath.Join(g.pause.parent, "stop-request.json"), []byte("uninterpreted original"), 0600)
					case "cancelled":
						cancel()
					}
				}
				return strings.Repeat("8", 64), nil
			}
			if _, err := g.execute(ctx, r, sha); err == nil || calls != 3 {
				t.Fatal("send boundary not reached/refused", err, calls)
			}
			g.admission = func(context.Context, receipt) (string, error) {
				t.Fatal("durable unknown intent retried admission")
				return "", errRefused
			}
			if _, err := g.execute(context.Background(), r, sha); err == nil {
				t.Fatal("durable intent gained resend permission")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 0 {
				t.Fatal("sent across changed boundary")
			}
		})
	}
}

func TestGatewayConcurrentRequestHoldsBothOriginalLocks(t *testing.T) {
	g, f, r, sha := gatewayFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g.admission = func(ctx context.Context, _ receipt) (string, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return strings.Repeat("8", 64), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := g.execute(ctx, r, sha); done <- e }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first request did not enter locked admission")
	}
	if !heldFixtureLock(filepath.Join(g.pause.parent, "pause.lock")) || !heldFixtureLock(filepath.Join(g.pause.parent, "operation.lock")) {
		t.Fatal("original lock lifetime lost")
	}
	if _, err := g.execute(ctx, r, sha); err == nil {
		t.Fatal("concurrent request acquired original owner")
	}
	if got := g.observe(ctx, r, sha); got.State == "gateway_stopped_drained" {
		t.Fatal("observer bypassed locked in-flight request")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 1 {
		t.Fatal("concurrent send", f.sends)
	}
}

func TestGatewayMissingEventRecoversOnlyOriginalEvent(t *testing.T) {
	g, f, r, sha := gatewayFixture(t)
	f.mu.Lock()
	f.mode = "missing_event"
	f.mu.Unlock()
	if out, err := gatewayWaitAtEvent(t, g, f, r, sha); err == nil || out.Job != 43 {
		t.Fatal("original wait did not cancel with exact job")
	}
	drainGatewaySignal(f.eventEntered)
	g.admission = func(context.Context, receipt) (string, error) { t.Fatal("fresh admission"); return "", errRefused }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := g.execute(ctx, r, sha); done <- e }()
	select {
	case <-f.eventEntered:
	case <-ctx.Done():
		t.Fatal("original event recovery did not subscribe")
	}
	if err := f.conn.Emit(pauseManagerPath, pauseManager+".Manager.JobRemoved", uint32(43), dbus.ObjectPath(pauseJobPath(43)), gatewayUnit, "done"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sends != 1 {
		t.Fatal("event recovery resent")
	}
}

func TestGatewayOriginalProcessAndCgroupConflicts(t *testing.T) {
	for _, mode := range []string{"uid", "gid", "status_missing", "pid_reused", "reused_zombie", "malformed_stat", "image", "live_deleted", "zombie_wrong_deleted", "zombie_cgroup_replaced", "zombie", "dead", "lower_dead", "zombie_deleted"} {
		t.Run(mode, func(t *testing.T) {
			g, f, _, _ := gatewayFixture(t)
			b, err := g.connect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer b.close()
			before, err := b.snapshot(context.Background(), nil)
			if err != nil || !gatewayInitial(before) {
				t.Fatal("baseline", err)
			}
			state, start, uid, gid := "S", "1234", uint32(24567), uint32(24568)
			switch mode {
			case "uid":
				uid++
			case "gid":
				gid++
			case "pid_reused":
				start = "1235"
			case "reused_zombie":
				state, start = "Z", "1235"
			case "zombie", "zombie_deleted", "zombie_wrong_deleted", "zombie_cgroup_replaced":
				state = "Z"
			case "dead":
				state = "X"
			case "lower_dead":
				state = "x"
			}
			gatewayProc(t, f.host, 9001, state, start, gatewayUnit, uid, gid, f.host.nginx)
			dir := filepath.Join(f.host.proc, "9001")
			switch mode {
			case "status_missing":
				testWrite(t, filepath.Join(dir, "status"), []byte("Uid: 24567 24567 24567 24567\n"), 0444)
			case "malformed_stat":
				testWrite(t, filepath.Join(dir, "stat"), []byte("9001 (bad) Z 0\n"), 0444)
			case "image":
				if err := os.Remove(filepath.Join(dir, "exe")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.host.controllerExe, filepath.Join(dir, "exe")); err != nil {
					t.Fatal(err)
				}
			case "live_deleted", "zombie_deleted":
				testWrite(t, filepath.Join(dir, "cgroup"), []byte("0::"+gatewayGroup+" (deleted)\n"), 0444)
			case "zombie_wrong_deleted":
				testWrite(t, filepath.Join(dir, "cgroup"), []byte("0::/system.slice/other.service (deleted)\n"), 0444)
			case "zombie_cgroup_replaced":
				original := filepath.Join(f.host.cgroup, gatewayGroup)
				if err := os.Rename(original, original+"-retained"); err != nil {
					t.Fatal(err)
				}
				testWrite(t, filepath.Join(original, "cgroup.events"), []byte("populated 0\n"), 0600)
				_, ino, _, err := serviceGroup(f.host.cgroup, gatewayGroup, false)
				if err != nil || ino == before.GroupInode {
					t.Fatal("fixture did not replace original cgroup")
				}
			}
			_, err = b.snapshot(context.Background(), &before)
			busy := mode == "zombie" || mode == "dead" || mode == "lower_dead" || mode == "zombie_deleted"
			if busy {
				if !errors.Is(err, errPauseBusy) {
					t.Fatalf("original reaping refused: %v", err)
				}
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.host.cgroup, gatewayGroup, "cgroup.events"), []byte("populated 0\n"), 0600); err != nil {
					t.Fatal(err)
				}
				current := before
				current.Main, current.Control = serviceProcess{}, serviceProcess{}
				if err := b.drained(context.Background(), before, current); err != nil {
					t.Fatal("original PID disappearance", err)
				}
			} else if !errors.Is(err, errRefused) {
				t.Fatalf("identity conflict hidden: %v", err)
			}
			if mode == "zombie_cgroup_replaced" && !errors.Is(b.drained(context.Background(), before, gatewayFacts{}), errRefused) {
				t.Fatal("busy process hid cgroup identity conflict")
			}
		})
	}
}

func TestGatewaySavedEvidenceRefusesRebinding(t *testing.T) {
	for _, mode := range []string{"producer", "capture", "pause", "execution", "boot", "partial", "null", "missing", "unknown", "symlink", "hardlink", "controller_after"} {
		t.Run(mode, func(t *testing.T) {
			g, f, r, sha := gatewayFixture(t)
			if _, err := g.execute(context.Background(), r, sha); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(g.pause.parent, "gateway-stop", "intent.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			original := string(raw)
			switch mode {
			case "producer":
				g.producer = strings.Repeat("a", 64)
			case "capture":
				r.ProducerSHA = strings.Repeat("a", 64)
			case "pause":
				g.pause.producer = strings.Repeat("a", 64)
			case "execution":
				sha = strings.Repeat("a", 64)
			case "boot":
				raw = bytes.Replace(raw, []byte(r.Observed.Host.BootID), []byte("ffffffff-ffff-4fff-8fff-ffffffffffff"), 1)
			case "partial":
				raw = raw[:len(raw)/2]
			case "null":
				raw = bytes.Replace(raw, []byte(`"before":{`), []byte(`"before":null,"discarded":{`), 1)
			case "missing":
				raw = bytes.Replace(raw, []byte(`"version":1,`), nil, 1)
			case "unknown":
				raw = append([]byte(`{"unexpected":true,`), raw[1:]...)
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(g.pause.parent, "retained-intent")); err != nil {
					t.Fatal(err)
				}
			case "controller_after":
				path = filepath.Join(g.pause.parent, "gateway-stop", "drain.json")
				raw, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.Replace(raw, []byte(`"pid":9002`), []byte(`"pid":9003`), 1)
			}
			if mode != "symlink" && string(raw) != original {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			g.admission = func(context.Context, receipt) (string, error) {
				t.Fatal("rebound original requested admission")
				return "", errRefused
			}
			if _, err := g.execute(context.Background(), r, sha); err == nil {
				t.Fatal("original evidence rebound", mode)
			}
			if out := g.observe(context.Background(), r, sha); out.State == "gateway_stopped_drained" {
				t.Fatal("diagnostic accepted conflicting original", mode)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.sends != 1 {
				t.Fatal("conflicting evidence resent")
			}
		})
	}
}

func TestGatewaySDKRetainsSecondSnapshotConflict(t *testing.T) {
	for _, mode := range []string{"second_job", "second_reactivation"} {
		t.Run(mode, func(t *testing.T) {
			g, f, r, sha := gatewayFixture(t)
			if _, err := g.execute(context.Background(), r, sha); err != nil {
				t.Fatal(err)
			}
			b, err := g.connect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer b.close()
			// Recover the independently stored original before facts, without
			// substituting a zero identity or a newly observed process.
			raw, err := os.ReadFile(filepath.Join(g.pause.parent, "gateway-stop/intent.json"))
			var saved gatewayIntent
			if err != nil || json.Unmarshal(raw, &saved) != nil {
				t.Fatal("original fixture", err)
			}
			f.mu.Lock()
			f.mode, f.gatewayReads = mode, 0
			f.mu.Unlock()
			fact, err := b.snapshot(context.Background(), &saved.Before)
			if !errors.Is(err, errPauseBusy) || !gatewayConflict(fact, 43) {
				t.Fatalf("second observed conflict lost: %+v %v", fact, err)
			}
			if mode == "second_job" && fact.Job != 99 || mode == "second_reactivation" && fact.Active != "active" {
				t.Fatal("old snapshot replaced current conflict", fact)
			}
		})
	}
}
