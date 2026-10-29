package lifecycle

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
)

type gatewayBus struct {
	*unitBus
	host gatewayHost
}

func connectGateway(ctx context.Context, socket string, h gatewayHost) (gatewayConnection, error) {
	b, err := connectUnitBus(ctx, socket)
	if err != nil {
		return nil, err
	}
	return &gatewayBus{b, h}, nil
}
func (b *gatewayBus) stopGateway(ctx context.Context) (pauseJob, error) {
	return b.stopUnit(ctx, gatewayUnit)
}
func (b *gatewayBus) event(ctx context.Context, j pauseJob) (pauseEvent, error) {
	return b.unitEvent(ctx, j, gatewayUnit)
}
func (b *gatewayBus) drained(ctx context.Context, a, c gatewayFacts) error {
	return b.host.drained(ctx, a, c)
}

func emptySuccessStatus(value any) bool {
	// systemd's (aiai) contains separately typed exit-code and signal arrays.
	values, ok := value.([]any)
	if !ok || len(values) != 2 {
		return false
	}
	for _, v := range values {
		codes, ok := v.([]int32)
		if !ok || len(codes) != 0 {
			return false
		}
	}
	return true
}

func serviceCommand(value any, executable string, args ...string) bool {
	rows, ok := value.([][]any)
	if !ok || len(rows) != 1 || len(rows[0]) != 10 {
		return false
	}
	v := rows[0]
	argv, ok := v[1].([]string)
	if !ok || len(argv) != len(args) || v[0] != executable || !pauseBool(v[2], false) {
		return false
	}
	for i := range args {
		if args[i] != argv[i] {
			return false
		}
	}
	for _, value := range v[3:7] {
		if _, ok := value.(uint64); !ok {
			return false
		}
	}
	if _, ok := v[7].(uint32); !ok {
		return false
	}
	for _, value := range v[8:] {
		if _, ok := value.(int32); !ok {
			return false
		}
	}
	return true
}

func knownServiceResult(result string) bool {
	switch result {
	case "success", "exit-code", "signal", "core-dump", "timeout", "watchdog", "start-limit-hit", "resources", "protocol", "oom-kill", "exec-condition", "skipped":
		return true
	}
	return false
}

func gatewayConfig(s map[string]any) error {
	group, gr := s["Group"].(string)
	if !gr || group != "" && group != "nucleiron-gateway" || s["TimeoutStopFailureMode"] != "terminate" {
		return errRefused
	}
	if s["Type"] != "simple" || s["Restart"] != "on-failure" || s["User"] != "nucleiron-gateway" || !pauseBool(s["DynamicUser"], true) || !pauseBool(s["RemainAfterExit"], false) || s["Slice"] != "system.slice" || s["KillMode"] != "control-group" || s["KillSignal"] != int32(15) || s["FinalKillSignal"] != int32(9) || !pauseBool(s["SendSIGKILL"], true) || !pauseBool(s["SendSIGHUP"], false) || !emptySuccessStatus(s["SuccessExitStatus"]) {
		return errRefused
	}
	timeout, ok := s["TimeoutStopUSec"].(uint64)
	if !ok || timeout == 0 || timeout == math.MaxUint64 {
		return errRefused
	}
	for _, key := range []string{"ExecStartPost", "ExecStop", "ExecStopPost", "ExecReload", "ExecCondition"} {
		rows, ok := s[key].([][]any)
		if !ok || len(rows) != 0 {
			return errRefused
		}
	}
	if !serviceCommand(s["ExecStart"], "/usr/sbin/nginx", "/usr/sbin/nginx", "-c", "/etc/nucleiron-gateway/gateway.conf") || !serviceCommand(s["ExecStartPre"], "/usr/sbin/nginx", "/usr/sbin/nginx", "-t", "-c", "/etc/nucleiron-gateway/gateway.conf") {
		return errRefused
	}
	return nil
}

func gatewayDependencies(u map[string]any) (map[string][]string, error) {
	out := map[string][]string{}
	for _, key := range []string{"Requires", "Wants", "Requisite"} {
		values, ok := u[key].([]string)
		if !ok || len(values) > 64 {
			return nil, errRefused
		}
		seen := map[string]bool{}
		for _, unit := range values {
			if unit == "" || seen[unit] {
				return nil, errRefused
			}
			seen[unit] = true
		}
		out[key] = append([]string{}, values...)
		sort.Strings(out[key])
	}
	return out, nil
}

func (b *gatewayBus) upstreams(ctx context.Context, u map[string]any) error {
	return b.loadedUpstreams(ctx, u, true)
}

func (b *gatewayBus) loadedUpstreams(ctx context.Context, u map[string]any, active bool) error {
	deps, err := gatewayDependencies(u)
	if err != nil {
		return err
	}
	requires, wants, requisite := deps["Requires"], deps["Wants"], deps["Requisite"]
	seen := map[string]bool{}
	for _, unit := range requires {
		if unit == "" || seen[unit] {
			return errRefused
		}
		seen[unit] = true
	}
	if !seen[controllerUnit] || !seen[firewallUnit] {
		return errRefused
	}
	for _, unit := range append(append(append([]string{}, requires...), wants...), requisite...) {
		if unit == "" {
			return errRefused
		}
		up, err := b.api.GetUnitPropertiesContext(ctx, unit)
		if err != nil || up["Id"] != unit || up["LoadState"] != "loaded" || !pauseBool(up["StopWhenUnneeded"], false) {
			return errRefused
		}
		if active && (unit == controllerUnit || unit == firewallUnit) && up["ActiveState"] != "active" {
			return errRefused
		}
	}
	return nil
}

func (b *gatewayBus) rawSnapshot(ctx context.Context, first bool) (gatewayFacts, error) {
	var out gatewayFacts
	u, err := b.api.GetUnitPropertiesContext(ctx, gatewayUnit)
	if err != nil {
		return out, err
	}
	out.Active, out.Sub, out.Invocation, out.Job, err = pauseUnit(u, gatewayUnit)
	if err != nil || !pauseStrings(u["RequiredBy"]) || !pauseStrings(u["RequisiteOf"]) || !pauseStrings(u["Triggers"]) || !pauseStrings(u["TriggeredBy"]) || u["SuccessAction"] != "none" || u["FailureAction"] != "none" || u["JobTimeoutAction"] != "none" || u["StartLimitAction"] != "none" {
		return out, errRefused
	}
	if first && b.upstreams(ctx, u) != nil {
		return out, errRefused
	}
	s, err := b.api.GetUnitTypePropertiesContext(ctx, gatewayUnit, "Service")
	if err != nil || gatewayConfig(s) != nil {
		return out, errRefused
	}
	main, m := s["MainPID"].(uint32)
	control, c := s["ControlPID"].(uint32)
	start, st := s["ExecMainStartTimestampMonotonic"].(uint64)
	result, re := s["Result"].(string)
	group, g := s["ControlGroup"].(string)
	if !m || !c || !st || !re || !g || !knownServiceResult(result) || group != "" && group != gatewayGroup || (main != 0 || control != 0) && (group == "" || out.Invocation == "" || start == 0) {
		return out, errRefused
	}
	out.Main.PID, out.Control.PID, out.StartUSec, out.Result, out.Group = main, control, start, result, gatewayGroup
	deps, err := gatewayDependencies(u)
	if err != nil {
		return out, err
	}
	config := []any{u["Id"], u["FragmentPath"], u["DropInPaths"], deps, s["User"], s["Group"], s["DynamicUser"], s["Type"], s["Restart"], s["KillMode"], s["KillSignal"], s["FinalKillSignal"], s["SendSIGKILL"], s["SendSIGHUP"], s["TimeoutStopUSec"], s["TimeoutStopFailureMode"]}
	raw, _ := json.Marshal(config)
	out.Configuration = bytesSHA(raw)
	out.Jobs = []uint32{}
	if out.Job != 0 {
		out.Jobs = append(out.Jobs, out.Job)
	}
	jobs, err := b.api.ListJobsContext(ctx)
	if err != nil || len(jobs) > 10000 {
		return out, errRefused
	}
	seen := map[uint32]bool{}
	found := out.Job == 0
	for _, job := range jobs {
		if job.Id == 0 || seen[job.Id] || string(job.JobPath) != pauseJobPath(job.Id) {
			return out, errRefused
		}
		seen[job.Id] = true
		if job.Unit == gatewayUnit {
			if job.Id != out.Job {
				return out, errRefused
			}
			found = true
		}
	}
	if !found {
		return out, errPauseBusy
	}
	return out, nil
}

func (b *gatewayBus) snapshot(ctx context.Context, original *gatewayFacts) (gatewayFacts, error) {
	before, err := b.rawSnapshot(ctx, original == nil)
	if original != nil && (err == nil || errors.Is(err, errPauseBusy)) && gatewayUnitIdentity(*original, before) != nil {
		return before, errRefused
	}
	if err != nil {
		return before, err
	}
	if err = b.host.enrich(&before, original); err != nil {
		return before, err
	}
	after, err := b.rawSnapshot(ctx, original == nil)
	if original != nil && (err == nil || errors.Is(err, errPauseBusy)) && gatewayUnitIdentity(*original, after) != nil {
		return after, errRefused
	}
	if err != nil {
		return after, err
	}
	if after.Main.PID == before.Main.PID {
		after.Main = before.Main
	}
	if after.Control.PID == before.Control.PID {
		after.Control = before.Control
	}
	after.GroupDevice, after.GroupInode = before.GroupDevice, before.GroupInode
	if !reflect.DeepEqual(before, after) {
		return after, errPauseBusy
	}
	return before, nil
}

func (b *gatewayBus) controller(ctx context.Context) (controllerFacts, error) {
	var out controllerFacts
	dependencies, err := b.api.GetUnitPropertiesContext(ctx, gatewayUnit)
	if err != nil || b.upstreams(ctx, dependencies) != nil {
		return out, errRefused
	}
	u, err := b.api.GetUnitPropertiesContext(ctx, controllerUnit)
	if err != nil || u["Id"] != controllerUnit || u["LoadState"] != "loaded" || u["ActiveState"] != "active" || u["SubState"] != "running" || !pauseBool(u["NeedDaemonReload"], false) || !pauseBool(u["StopWhenUnneeded"], false) {
		return out, errRefused
	}
	invocation, iv := u["InvocationID"].([]byte)
	job, err := pauseJobTuple(u["Job"])
	if err != nil || job != 0 || !iv || len(invocation) != 16 || u["FragmentPath"] != "/etc/systemd/system/"+controllerUnit {
		return out, errRefused
	}
	inv := hex.EncodeToString(invocation)
	paths, pathsOK := u["DropInPaths"].([]string)
	if !pathsOK || len(paths) > 64 {
		return out, errRefused
	}
	s, err := b.api.GetUnitTypePropertiesContext(ctx, controllerUnit, "Service")
	if err != nil || s["Type"] != "simple" || s["User"] != "nucleiron" || s["Group"] != "nucleiron" || !pauseBool(s["DynamicUser"], false) || !serviceCommand(s["ExecStart"], "/opt/nucleus-halo/bin/garm", "/opt/nucleus-halo/bin/garm", "-config", controllerConfig) {
		return out, errRefused
	}
	pid, p := s["MainPID"].(uint32)
	control, c := s["ControlPID"].(uint32)
	start, st := s["ExecMainStartTimestampMonotonic"].(uint64)
	if !p || !c || !st || pid == 0 || control != 0 || start == 0 || s["ControlGroup"] != "/system.slice/"+controllerUnit {
		return out, errRefused
	}
	process, err := b.host.process(pid, controllerUnit, nil)
	if err != nil || process.PID == 0 {
		return out, errRefused
	}
	raw, _ := json.Marshal([]any{u["FragmentPath"], u["DropInPaths"], s["Type"], s["User"], s["Group"], s["DynamicUser"]})
	out = controllerFacts{inv, start, process, bytesSHA(raw)}
	jobs, err := b.api.ListJobsContext(ctx)
	if err != nil || len(jobs) > 10000 {
		return controllerFacts{}, errRefused
	}
	for _, job := range jobs {
		if job.Unit == controllerUnit {
			return controllerFacts{}, errRefused
		}
	}
	return out, nil
}
