package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type sessionWindowTestManager struct {
	mu    sync.Mutex
	conn  *dbus.Conn
	mode  string
	calls int
	ready bool
}

type sessionControllerTestProperties struct {
	mu      sync.Mutex
	mode    string
	stopped bool
}

type sessionEnvironmentFile struct {
	Path   string
	Ignore bool
}

func (p *sessionControllerTestProperties) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw := map[string]any{}
	if iface == pauseManager+".Unit" {
		raw = map[string]any{"Id": controllerUnit, "LoadState": "loaded", "FragmentPath": "/etc/systemd/system/" + controllerUnit, "DropInPaths": []string{"/etc/systemd/system/" + controllerUnit + ".d/30-provider-guard.conf"}, "InvocationID": []byte("controller-00001"), "ActiveState": "active", "SubState": "running", "Job": pauseTestJob{0, "/"}, "BindsTo": []string{"var-log-nucleiron-guests.mount"}, "RequiredBy": []string{gatewayUnit}}
		for _, name := range []string{"NeedDaemonReload", "StopWhenUnneeded", "RefuseManualStart", "RefuseManualStop"} {
			raw[name] = false
		}
		for _, name := range []string{"Requires", "Wants", "Requisite", "OnFailure", "OnSuccess", "PartOf", "BoundBy", "ConsistsOf", "PropagatesStopTo", "StopPropagatedFrom", "UpheldBy", "Upholds", "RequisiteOf", "Triggers", "TriggeredBy"} {
			raw[name] = []string{}
		}
		for _, name := range []string{"SuccessAction", "FailureAction", "StartLimitAction", "JobTimeoutAction"} {
			raw[name] = "none"
		}
		if p.stopped {
			raw["ActiveState"], raw["SubState"], raw["InvocationID"] = "inactive", "dead", []byte{}
		}
		switch p.mode {
		case "reboot":
			raw["FailureAction"] = "reboot"
		case "reload":
			raw["NeedDaemonReload"] = true
		case "hook":
			raw["OnSuccess"] = []string{"other.service"}
		case "dropin":
			raw["DropInPaths"] = []string{}
		case "bound":
			raw["BindsTo"] = []string{"other.service"}
		}
	} else if iface == pauseManager+".Service" {
		raw = map[string]any{"Type": "simple", "Restart": "on-failure", "User": "nucleiron", "Group": "nucleiron", "DynamicUser": false, "RemainAfterExit": false, "Slice": "system.slice", "KillMode": "control-group", "KillSignal": int32(15), "FinalKillSignal": int32(9), "SendSIGKILL": true, "SendSIGHUP": false, "TimeoutStopUSec": uint64(90000000), "TimeoutStopFailureMode": "terminate", "SupplementaryGroups": []string{"lxd"}, "Environment": []string{}, "PassEnvironment": []string{}, "EnvironmentFiles": []sessionEnvironmentFile{}, "SuccessExitStatus": pauseTestExitStatus{[]int32{}, []int32{}}, "MainPID": uint32(9002), "ControlPID": uint32(0), "ExecMainStartTimestampMonotonic": uint64(100), "Result": "success", "ControlGroup": "/system.slice/" + controllerUnit}
		for _, name := range []string{"ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload", "ExecCondition"} {
			raw[name] = []pauseTestExec{}
		}
		raw["ExecStart"] = []pauseTestExec{{Path: "/opt/nucleus-halo/bin/garm", Args: []string{"/opt/nucleus-halo/bin/garm", "-config", controllerConfig}}}
		raw["ExecStartPre"] = []pauseTestExec{{Path: "/opt/nucleus-halo/bin/guarded-provider", Args: []string{"/opt/nucleus-halo/bin/guarded-provider", "--check-startup"}}}
		if p.stopped {
			raw["MainPID"] = uint32(0)
		}
		switch p.mode {
		case "old_python":
			raw["ExecStartPre"] = []pauseTestExec{{Path: "/opt/nucleus-halo/bin/guarded-lxd-provider", Args: []string{"/opt/nucleus-halo/bin/guarded-lxd-provider", "--check-startup"}}}
		case "environment":
			raw["EnvironmentFiles"] = []sessionEnvironmentFile{{"/unexpected", false}}
		case "kill":
			raw["KillMode"] = "process"
		case "success_status":
			raw["SuccessExitStatus"] = pauseTestExitStatus{[]int32{1}, []int32{}}
		case "command":
			raw["ExecStart"] = []pauseTestExec{{Path: "/opt/nucleus-halo/bin/garm", Args: []string{"/opt/nucleus-halo/bin/garm", "-config", "/unexpected"}}}
		}
	} else {
		return nil, dbus.MakeFailedError(errors.New("unexpected interface"))
	}
	out := map[string]dbus.Variant{}
	for k, v := range raw {
		out[k] = dbus.MakeVariant(v)
	}
	return out, nil
}

func TestSessionControllerReadsLoadedUnitAndDrainsOriginalProcess(t *testing.T) {
	f, socket, ph := privatePauseBus(t)
	properties := &sessionControllerTestProperties{}
	path := dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + strings.ReplaceAll(controllerUnit, ".", "_2e"))
	if err := f.conn.Export(properties, path, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wire, err := connectUnitBus(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer wire.close()
	h := gatewayHost{proc: ph.proc, cgroup: ph.cgroup, controllerExe: filepath.Join(t.TempDir(), "garm")}
	testWrite(t, h.controllerExe, []byte("synthetic GARM image for loaded-unit tests"), 0755)
	gatewayProc(t, h, 9002, "S", "3456", controllerUnit, 24567, 24568, h.controllerExe)
	testWrite(t, filepath.Join(h.proc, "9002", "cmdline"), []byte("/opt/nucleus-halo/bin/garm\x00-config\x00"+controllerConfig+"\x00"), 0444)
	testWrite(t, filepath.Join(h.cgroup, "/system.slice/"+controllerUnit, "cgroup.events"), []byte("populated 1\n"), 0600)
	b := &sessionBus{unitBus: wire, host: h, uid: 24567, gid: 24568}
	before, err := b.controller(ctx, nil)
	if err != nil || !sessionControllerRunning(before) {
		t.Fatalf("loaded controller contract rejected: %v", err)
	}
	for _, mode := range []string{"reboot", "reload", "hook", "dropin", "bound", "old_python", "environment", "kill", "success_status", "command"} {
		t.Run(mode, func(t *testing.T) {
			properties.mu.Lock()
			properties.mode = mode
			properties.mu.Unlock()
			if _, err := b.controller(ctx, nil); err == nil {
				t.Fatal("unsafe loaded controller accepted")
			}
		})
	}
	properties.mu.Lock()
	properties.mode = ""
	properties.stopped = true
	properties.mu.Unlock()
	if os.RemoveAll(filepath.Join(h.proc, "9002")) != nil {
		t.Fatal("fixture process removal")
	}
	testWrite(t, filepath.Join(h.cgroup, "/system.slice/"+controllerUnit, "cgroup.events"), []byte("populated 0\n"), 0600)
	current, err := b.controller(ctx, &before)
	if err != nil || !sessionControllerStopped(before, current) || b.controllerDrained(ctx, before, current) != nil {
		t.Fatalf("original process drain failed: %v", err)
	}
	gatewayProc(t, h, 9002, "S", "9999", controllerUnit, 24567, 24568, h.controllerExe)
	if b.controllerDrained(ctx, before, current) == nil {
		t.Fatal("PID reuse accepted as the original process drain")
	}
}

func (m *sessionWindowTestManager) Subscribe() *dbus.Error {
	m.mu.Lock()
	m.ready = true
	m.mu.Unlock()
	return nil
}
func (m *sessionWindowTestManager) StopUnit(unit, mode string) (dbus.ObjectPath, *dbus.Error) {
	if unit != pauseTimer && unit != controllerUnit {
		return "", dbus.MakeFailedError(errors.New("unexpected stop target"))
	}
	id := uint32(41)
	stage := "watchdog"
	if unit == controllerUnit {
		id, stage = 42, "stop"
	}
	return m.job(unit, mode, stage, id)
}
func (m *sessionWindowTestManager) StartUnit(unit, mode string) (dbus.ObjectPath, *dbus.Error) {
	if unit != controllerUnit {
		return "", dbus.MakeFailedError(errors.New("unexpected start target"))
	}
	return m.job(unit, mode, "start", 43)
}
func (m *sessionWindowTestManager) job(unit, mode, stage string, id uint32) (dbus.ObjectPath, *dbus.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready || mode != "fail" {
		return "", dbus.MakeFailedError(errors.New("missing subscription or wrong mode"))
	}
	m.calls++
	path := dbus.ObjectPath(pauseJobPath(id))
	// The signal precedes the method reply, as it may on real systemd.
	if err := m.conn.Emit(pauseManagerPath, pauseManager+".Manager.JobRemoved", id, path, unit, "done"); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	if m.mode == stage+"_lost" {
		return "", dbus.MakeFailedError(errors.New("synthetic lost method reply"))
	}
	return path, nil
}

type sessionWindowWireBus struct {
	*sessionWindowFixtureBus
	wire *unitBus
}

func (b sessionWindowWireBus) stopTimer(ctx context.Context) (pauseJob, error) {
	b.sessionWindowFixtureBus.stopTimer(ctx)
	return b.wire.stopUnit(ctx, pauseTimer)
}
func (b sessionWindowWireBus) stopController(ctx context.Context) (pauseJob, error) {
	b.sessionWindowFixtureBus.stopController(ctx)
	return b.wire.stopUnit(ctx, controllerUnit)
}
func (b sessionWindowWireBus) startController(ctx context.Context) (pauseJob, error) {
	b.sessionWindowFixtureBus.startController(ctx)
	return b.wire.startController(ctx)
}
func (b sessionWindowWireBus) event(ctx context.Context, j pauseJob, unit string) (pauseEvent, error) {
	return b.wire.unitEvent(ctx, j, unit)
}

func TestSessionWindowPrivateDBusOriginalJobsAndEarlyEvents(t *testing.T) {
	for _, mode := range []string{"ok", "watchdog_lost", "stop_lost", "start_lost"} {
		t.Run(mode, func(t *testing.T) {
			w, p, raw, facts := sessionWindowFixture(t)
			fixture, socket, _ := privatePauseBus(t)
			manager := &sessionWindowTestManager{conn: fixture.conn, mode: mode}
			if err := fixture.conn.Export(manager, pauseManagerPath, pauseManager+".Manager"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			wire, err := connectUnitBus(ctx, socket)
			if err != nil {
				t.Fatal(err)
			}
			defer wire.close()
			facts.manager, err = wire.identity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			w.services = sessionWindowWireBus{facts, wire}
			_, err = w.execute(ctx, p, raw)
			if (err == nil) != (mode == "ok") {
				t.Fatalf("private D-Bus result mode=%s: %v", mode, err)
			}
			manager.mu.Lock()
			calls := manager.calls
			manager.mode = "ok"
			manager.mu.Unlock()
			_, err = w.execute(ctx, p, raw)
			if (err == nil) != (mode == "ok") {
				t.Fatal("lost original method reply became a successful recovery")
			}
			manager.mu.Lock()
			defer manager.mu.Unlock()
			if calls == 0 || manager.calls != calls || mode == "ok" && calls != 3 {
				t.Fatal("window resent a real D-Bus service request")
			}
		})
	}
}
