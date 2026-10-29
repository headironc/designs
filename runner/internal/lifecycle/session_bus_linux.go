package lifecycle

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

type sessionBus struct {
	*unitBus
	host     gatewayHost
	uid, gid uint32
}

func (b *sessionBus) listener(ctx context.Context, process serviceProcess) (sessionSocket, error) {
	return b.host.listener(ctx, process, 9997)
}

// Retirement makes no fresh credential to authenticate its expired token's
// rejection. Bind the original serving process and listener instead.
func (b *sessionBus) retirementTarget(ctx context.Context, p sessionPlan, issued sessionIntent, ready sessionWindowReady) error {
	if ctx.Err() != nil || !issued.Origin.valid(p) || issued.Origin.Invocation != ready.After.Invocation || issued.Origin.ConfigSHA != ready.ConfigSHA || !ready.Listener.valid() {
		return errRefused
	}
	boot, _, err := b.identity(ctx)
	if err != nil || boot != p.Boot {
		return errRefused
	}
	check := func() error {
		current, err := b.controller(ctx, nil)
		if err != nil || !equalSessionController(current, ready.After) {
			return errRefused
		}
		listener, err := b.listener(ctx, current.Main)
		if err != nil || listener != ready.Listener {
			return errRefused
		}
		config, raw, err := pinDatabaseConfiguration(protectedFiles{uid: 0, boundary: "/"}, controllerConfig, b.gid)
		if err != nil {
			return errRefused
		}
		config.Close()
		if bytesSHA(raw) != issued.Origin.ConfigSHA {
			return errRefused
		}
		boot, _, err := b.identity(ctx)
		if err != nil || boot != p.Boot {
			return errRefused
		}
		return nil
	}
	if check() != nil || check() != nil {
		return errRefused
	}
	return nil
}

func connectSessionBus(ctx context.Context) (*sessionBus, error) {
	uid, gid, err := serviceIdentity()
	if err != nil {
		return nil, errRefused
	}
	wire, err := connectUnitBus(ctx, "/run/dbus/system_bus_socket")
	if err != nil {
		return nil, errRefused
	}
	return &sessionBus{unitBus: wire, uid: uid, gid: gid, host: gatewayHost{proc: "/proc", cgroup: "/sys/fs/cgroup", realFS: true, controllerExe: "/opt/nucleus-halo/bin/garm"}}, nil
}

func (b *sessionBus) identity(ctx context.Context) (string, string, error) {
	boot, err := (pauseHost{proc: b.host.proc, cgroup: b.host.cgroup, realFS: b.host.realFS}).boot()
	if err != nil {
		return "", "", errRefused
	}
	manager, err := b.unitBus.identity(ctx)
	return boot, manager, err
}
func (b *sessionBus) watchdog(ctx context.Context, original *pauseUnits) (pauseUnits, error) {
	return (&pauseBus{unitBus: b.unitBus, host: pauseHost{proc: b.host.proc, cgroup: b.host.cgroup, realFS: b.host.realFS}}).snapshot(ctx, original)
}
func (b *sessionBus) stopTimer(ctx context.Context) (pauseJob, error) {
	return b.stopUnit(ctx, pauseTimer)
}
func (b *sessionBus) stopController(ctx context.Context) (pauseJob, error) {
	return b.stopUnit(ctx, controllerUnit)
}
func (b *sessionBus) event(ctx context.Context, job pauseJob, unit string) (pauseEvent, error) {
	if unit != controllerUnit && unit != pauseTimer {
		return pauseEvent{}, errRefused
	}
	return b.unitEvent(ctx, job, unit)
}
func (b *sessionBus) watchdogDrained(ctx context.Context, before, current pauseUnits) error {
	return (pauseHost{proc: b.host.proc, cgroup: b.host.cgroup, realFS: b.host.realFS}).drained(ctx, before, current)
}

func sessionControllerConfiguration(u, s map[string]any) (string, error) {
	inv, i := u["InvocationID"].([]byte)
	if u["Id"] != controllerUnit || u["LoadState"] != "loaded" || u["FragmentPath"] != "/etc/systemd/system/"+controllerUnit || !i || len(inv) != 0 && len(inv) != 16 || !pauseStrings(u["DropInPaths"], "/etc/systemd/system/"+controllerUnit+".d/30-provider-guard.conf") {
		return "", errRefused
	}
	for _, field := range []string{"NeedDaemonReload", "StopWhenUnneeded", "RefuseManualStart", "RefuseManualStop"} {
		if !pauseBool(u[field], false) {
			return "", errRefused
		}
	}
	for _, field := range []string{"OnFailure", "OnSuccess", "PartOf", "BoundBy", "ConsistsOf", "PropagatesStopTo", "StopPropagatedFrom", "UpheldBy", "Upholds", "RequisiteOf", "Triggers", "TriggeredBy"} {
		if !pauseStrings(u[field]) {
			return "", errRefused
		}
	}
	if !pauseStrings(u["BindsTo"], "var-log-nucleiron-guests.mount") {
		return "", errRefused
	}
	// The dedicated gateway may require this controller, but it must already
	// be drained by the window's independent consumer admission.
	if !pauseStrings(u["RequiredBy"]) && !pauseStrings(u["RequiredBy"], gatewayUnit) {
		return "", errRefused
	}
	for _, field := range []string{"SuccessAction", "FailureAction", "StartLimitAction", "JobTimeoutAction"} {
		if u[field] != "none" {
			return "", errRefused
		}
	}
	if s["Type"] != "simple" || s["Restart"] != "on-failure" || s["User"] != "nucleiron" || s["Group"] != "nucleiron" || !pauseBool(s["DynamicUser"], false) || !pauseBool(s["RemainAfterExit"], false) || s["Slice"] != "system.slice" || s["KillMode"] != "control-group" || s["KillSignal"] != int32(15) || s["FinalKillSignal"] != int32(9) || !pauseBool(s["SendSIGKILL"], true) || !pauseBool(s["SendSIGHUP"], false) || !emptySuccessStatus(s["SuccessExitStatus"]) {
		return "", errRefused
	}
	timeout, ok := s["TimeoutStopUSec"].(uint64)
	if !ok || timeout == 0 || timeout == math.MaxUint64 || s["TimeoutStopFailureMode"] != "terminate" || !pauseStrings(s["SupplementaryGroups"], "lxd") || !pauseStrings(s["Environment"]) || !pauseStrings(s["PassEnvironment"]) {
		return "", errRefused
	}
	if files, ok := s["EnvironmentFiles"].([][]any); !ok || len(files) != 0 {
		return "", errRefused
	}
	for _, field := range []string{"ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload", "ExecCondition"} {
		rows, ok := s[field].([][]any)
		if !ok || len(rows) != 0 {
			return "", errRefused
		}
	}
	if !serviceCommand(s["ExecStart"], "/opt/nucleus-halo/bin/garm", "/opt/nucleus-halo/bin/garm", "-config", controllerConfig) || !serviceCommand(s["ExecStartPre"], "/opt/nucleus-halo/bin/guarded-provider", "/opt/nucleus-halo/bin/guarded-provider", "--check-startup") {
		return "", errRefused
	}
	deps, err := gatewayDependencies(u)
	if err != nil {
		return "", errRefused
	}
	facts := []any{u["FragmentPath"], u["DropInPaths"], deps, s["Type"], s["Restart"], s["User"], s["Group"], s["DynamicUser"], s["RemainAfterExit"], s["KillMode"], s["KillSignal"], s["FinalKillSignal"], s["SendSIGKILL"], s["SendSIGHUP"], timeout, s["TimeoutStopFailureMode"], s["SupplementaryGroups"], s["Environment"], s["PassEnvironment"]}
	raw, _ := json.Marshal(facts)
	return bytesSHA(raw), nil
}

func (b *sessionBus) controllerRaw(ctx context.Context) (gatewayFacts, error) {
	out := gatewayFacts{Group: "/system.slice/" + controllerUnit, Jobs: []uint32{}}
	u, err := b.api.GetUnitPropertiesContext(ctx, controllerUnit)
	if err != nil {
		return out, errRefused
	}
	s, err := b.api.GetUnitTypePropertiesContext(ctx, controllerUnit, "Service")
	if err != nil {
		return out, errRefused
	}
	out.Configuration, err = sessionControllerConfiguration(u, s)
	if err != nil {
		return out, errRefused
	}
	var a, sub, result, main, control, start, group bool
	out.Active, a = u["ActiveState"].(string)
	out.Sub, sub = u["SubState"].(string)
	out.Result, result = s["Result"].(string)
	out.Main.PID, main = s["MainPID"].(uint32)
	out.Control.PID, control = s["ControlPID"].(uint32)
	out.StartUSec, start = s["ExecMainStartTimestampMonotonic"].(uint64)
	g, group := s["ControlGroup"].(string)
	if !a || !sub || !result || !main || !control || !start || !group || !knownServiceResult(out.Result) || g != "" && g != out.Group || (out.Main.PID != 0 || out.Control.PID != 0) && g != out.Group {
		return out, errRefused
	}
	out.Invocation = hex.EncodeToString(u["InvocationID"].([]byte))
	out.Job, err = pauseJobTuple(u["Job"])
	if err != nil {
		return out, errRefused
	}
	jobs, err := b.api.ListJobsContext(ctx)
	if err != nil || len(jobs) > 10000 {
		return out, errRefused
	}
	for _, job := range jobs {
		if job.Unit == controllerUnit {
			if job.Id == 0 || string(job.JobPath) != pauseJobPath(job.Id) || len(out.Jobs) != 0 || job.Id != out.Job {
				return out, errRefused
			}
			out.Jobs = append(out.Jobs, job.Id)
		}
	}
	if out.Job != 0 && len(out.Jobs) != 1 {
		return out, errRefused
	}
	return out, nil
}

func (b *sessionBus) controller(ctx context.Context, original *gatewayFacts) (gatewayFacts, error) {
	first, err := b.controllerRaw(ctx)
	if err != nil || first.Control.PID != 0 || b.uid == 0 || b.gid == 0 {
		return first, errRefused
	}
	var old *serviceProcess
	if original != nil && first.Main.PID != 0 {
		old = &original.Main
	}
	process, err := b.host.process(first.Main.PID, controllerUnit, old)
	if err != nil || first.Main.PID != 0 && (process.PID == 0 || process.UID != b.uid || process.GID != b.gid || b.command(process) != nil) {
		return first, errRefused
	}
	dev, ino, _, err := serviceGroup(b.host.cgroup, first.Group, b.host.realFS)
	if err != nil {
		return first, errRefused
	}
	last, err := b.controllerRaw(ctx)
	if err != nil || !equalSessionController(first, last) {
		return first, errRefused
	}
	first.Main, first.GroupDevice, first.GroupInode = process, dev, ino
	return first, nil
}

func (b *sessionBus) command(process serviceProcess) error {
	path := filepath.Join(b.host.proc, strconv.FormatUint(uint64(process.PID), 10))
	dir, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return errRefused
	}
	defer dir.Close()
	raw, err := readAt(dir, "cmdline", 128*1024)
	if err != nil || !controllerCommand(raw) || !samePath(dir, path) {
		return errRefused
	}
	again, err := b.host.process(process.PID, controllerUnit, &process)
	if err != nil || again != process || !samePath(dir, path) {
		return errRefused
	}
	return nil
}

func (b *sessionBus) controllerDrained(ctx context.Context, before, current gatewayFacts) error {
	if ctx.Err() != nil || !sessionControllerStopped(before, current) {
		return errRefused
	}
	old, err := b.host.process(before.Main.PID, controllerUnit, &before.Main)
	if err != nil || old.PID != 0 {
		return errRefused
	}
	dev, ino, empty, err := serviceGroup(b.host.cgroup, before.Group, b.host.realFS)
	if err != nil || !empty || dev != current.GroupDevice || ino != current.GroupInode || dev != 0 && (dev != before.GroupDevice || ino != before.GroupInode) {
		return errRefused
	}
	return nil
}
