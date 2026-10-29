package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

const pauseManager = "org.freedesktop.systemd1"
const pauseManagerPath = dbus.ObjectPath("/org/freedesktop/systemd1")

type pauseBus struct {
	*unitBus
	host pauseHost
}

type unitBus struct {
	api            *systemd.Conn
	control        *dbus.Conn
	signals        chan *dbus.Signal
	owner, manager string
	stops          []func() bool
}

func connectPause(ctx context.Context, socket string, host pauseHost) (pauseConnection, error) {
	b, err := connectUnitBus(ctx, socket)
	if err != nil {
		return nil, err
	}
	return &pauseBus{b, host}, nil
}

func connectUnitBus(ctx context.Context, socket string) (*unitBus, error) {
	b := &unitBus{signals: make(chan *dbus.Signal, 64)}
	var connections []*dbus.Conn
	api, err := systemd.NewConnection(func() (*dbus.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { raw.Close() })
		b.stops = append(b.stops, stop)
		c, err := dbus.NewConn(raw, dbus.WithContext(ctx))
		if err != nil {
			raw.Close()
			return nil, err
		}
		if err = c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err == nil {
			err = c.Hello()
		}
		if err != nil {
			c.Close()
			return nil, err
		}
		connections = append(connections, c)
		return c, nil
	})
	if err != nil {
		for _, s := range b.stops {
			s()
		}
		return nil, err
	}
	b.api = api
	if len(connections) != 2 {
		b.close()
		return nil, errRefused
	}
	b.control = connections[0]
	b.manager, err = b.identity(ctx)
	if err != nil {
		b.close()
		return nil, err
	}
	b.owner = managerOwner(b.manager)
	connections[1].Signal(b.signals)
	// The SDK's initial AddMatch ignores its error. Check this consumer's own
	// match and subscription before allowing its one StopUnit request.
	err = connections[1].AddMatchSignalContext(ctx, dbus.WithMatchSender(b.owner), dbus.WithMatchObjectPath(pauseManagerPath), dbus.WithMatchInterface(pauseManager+".Manager"), dbus.WithMatchMember("JobRemoved"))
	if err == nil {
		err = connections[1].Object(b.owner, pauseManagerPath).CallWithContext(ctx, pauseManager+".Manager.Subscribe", dbus.FlagNoAutoStart).Err
	}
	if err != nil {
		b.close()
		return nil, err
	}
	return b, nil
}
func (b *unitBus) close() {
	if b.api != nil {
		b.api.Close()
	}
	for _, stop := range b.stops {
		stop()
	}
}
func (b *unitBus) identity(ctx context.Context) (string, error) {
	var owner, id string
	if b.control.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetId", 0).Store(&id) != nil || b.control.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, pauseManager).Store(&owner) != nil {
		return "", errRefused
	}
	result := id + "/" + owner
	if managerOwner(result) == "" {
		return "", errRefused
	}
	if b.manager != "" && b.manager != result {
		return "", errRefused
	}
	return result, nil
}
func (b *pauseBus) stopTimer(ctx context.Context) (pauseJob, error) {
	return b.stopUnit(ctx, pauseTimer)
}

func (b *unitBus) stopUnit(ctx context.Context, unit string) (pauseJob, error) {
	return b.unitJob(ctx, unit, "StopUnit")
}

func (b *unitBus) startController(ctx context.Context) (pauseJob, error) {
	return b.unitJob(ctx, controllerUnit, "StartUnit")
}

func (b *unitBus) unitJob(ctx context.Context, unit, method string) (pauseJob, error) {
	if method != "StopUnit" && (method != "StartUnit" || unit != controllerUnit) {
		return pauseJob{}, errRefused
	}
	// Preserve and validate the actual returned object path. The convenience
	// SDK wrapper reduces it to an int, which cannot retain this wire evidence.
	call := b.control.Object(b.owner, pauseManagerPath).CallWithContext(ctx, pauseManager+".Manager."+method, dbus.FlagNoAutoStart, unit, "fail")
	if call.Err != nil || len(call.Body) != 1 {
		return pauseJob{}, errRefused
	}
	path, ok := call.Body[0].(dbus.ObjectPath)
	if !ok {
		return pauseJob{}, errRefused
	}
	const prefix = "/org/freedesktop/systemd1/job/"
	raw := string(path)
	if len(raw) <= len(prefix) || raw[:len(prefix)] != prefix {
		return pauseJob{}, errRefused
	}
	id, err := strconv.ParseUint(raw[len(prefix):], 10, 32)
	if err != nil || id == 0 || pauseJobPath(uint32(id)) != raw {
		return pauseJob{}, errRefused
	}
	return pauseJob{ID: uint32(id), Path: raw}, nil
}
func (b *pauseBus) event(ctx context.Context, job pauseJob) (pauseEvent, error) {
	return b.unitEvent(ctx, job, pauseTimer)
}

func (b *unitBus) unitEvent(ctx context.Context, job pauseJob, expectedUnit string) (pauseEvent, error) {
	for {
		select {
		case <-ctx.Done():
			return pauseEvent{}, ctx.Err()
		case signal, ok := <-b.signals:
			if !ok {
				return pauseEvent{}, errRefused
			}
			if signal.Sender != b.owner || signal.Path != pauseManagerPath || signal.Name != pauseManager+".Manager.JobRemoved" {
				continue
			}
			if len(signal.Body) != 4 {
				return pauseEvent{}, errRefused
			}
			id, i := signal.Body[0].(uint32)
			path, p := signal.Body[1].(dbus.ObjectPath)
			unit, u := signal.Body[2].(string)
			result, r := signal.Body[3].(string)
			if !i || !p || !u || !r {
				return pauseEvent{}, errRefused
			}
			if id != job.ID {
				continue
			}
			if string(path) != job.Path || unit != expectedUnit || !pauseJobResult(result) {
				return pauseEvent{}, errRefused
			}
			return pauseEvent{ID: id, Path: string(path), Unit: unit, Sender: b.owner, Result: result}, nil
		}
	}
}

func pauseStrings(v any, wanted ...string) bool {
	values, ok := v.([]string)
	if !ok || len(values) != len(wanted) {
		return false
	}
	got := append([]string{}, values...)
	want := append([]string{}, wanted...)
	sort.Strings(got)
	sort.Strings(want)
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
func pauseBool(v any, want bool) bool { value, ok := v.(bool); return ok && value == want }
func pauseJobTuple(v any) (uint32, error) {
	pair, ok := v.([]any)
	if !ok || len(pair) != 2 {
		return 0, errRefused
	}
	id, ok := pair[0].(uint32)
	path, po := pair[1].(dbus.ObjectPath)
	if !ok || !po || id == 0 && path != "/" || id != 0 && string(path) != pauseJobPath(id) {
		return 0, errRefused
	}
	return id, nil
}

func pauseUnit(u map[string]any, name string) (string, string, string, uint32, error) {
	active, a := u["ActiveState"].(string)
	sub, s := u["SubState"].(string)
	inv, i := u["InvocationID"].([]byte)
	if u["Id"] != name || u["LoadState"] != "loaded" || u["FragmentPath"] != "/etc/systemd/system/"+name || !a || !s || !i || len(inv) != 0 && len(inv) != 16 || !pauseStrings(u["DropInPaths"]) || !pauseBool(u["NeedDaemonReload"], false) || !pauseBool(u["StopWhenUnneeded"], false) || !pauseBool(u["RefuseManualStop"], false) {
		return "", "", "", 0, errRefused
	}
	for _, field := range []string{"PartOf", "BindsTo", "BoundBy", "ConsistsOf", "PropagatesStopTo", "StopPropagatedFrom", "OnFailure", "OnSuccess", "UpheldBy", "Upholds"} {
		if !pauseStrings(u[field]) {
			return "", "", "", 0, errRefused
		}
	}
	job, err := pauseJobTuple(u["Job"])
	return active, sub, hex.EncodeToString(inv), job, err
}
func pauseTimerConfig(t map[string]any) error {
	if t["Unit"] != pauseWorker || !pauseBool(t["Persistent"], false) || !pauseBool(t["WakeSystem"], false) || !pauseBool(t["OnClockChange"], false) || !pauseBool(t["OnTimezoneChange"], false) || t["AccuracyUSec"] != uint64(1000000) || t["RandomizedDelayUSec"] != uint64(0) {
		return errRefused
	}
	cal, ok := t["TimersCalendar"].([][]any)
	if !ok || len(cal) != 0 {
		return errRefused
	}
	timers, ok := t["TimersMonotonic"].([][]any)
	if !ok || len(timers) != 2 {
		return errRefused
	}
	seen := map[string]bool{}
	for _, timer := range timers {
		if len(timer) != 3 {
			return errRefused
		}
		base, bo := timer[0].(string)
		value, vo := timer[1].(uint64)
		_, no := timer[2].(uint64)
		if !bo || !vo || !no || seen[base] || base != "OnBootUSec" && base != "OnUnitActiveUSec" || base == "OnBootUSec" && value != 10000000 || base == "OnUnitActiveUSec" && value != 5000000 {
			return errRefused
		}
		seen[base] = true
	}
	return nil
}
func pauseServiceConfig(s map[string]any) error {
	if s["Type"] != "oneshot" || s["Restart"] != "no" || !pauseBool(s["RemainAfterExit"], false) || s["Slice"] != "system.slice" || s["User"] != "" && s["User"] != "root" || s["Group"] != "" && s["Group"] != "root" {
		return errRefused
	}
	if !emptySuccessStatus(s["SuccessExitStatus"]) {
		return errRefused
	}
	for _, key := range []string{"ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "ExecCondition"} {
		rows, ok := s[key].([][]any)
		if !ok || len(rows) != 0 {
			return errRefused
		}
	}
	rows, ok := s["ExecStart"].([][]any)
	if !ok || len(rows) != 1 || len(rows[0]) != 10 {
		return errRefused
	}
	v := rows[0]
	if v[0] != "/opt/nucleus-halo/bin/provider-watchdog" || !pauseStrings(v[1], "/opt/nucleus-halo/bin/provider-watchdog") || !pauseBool(v[2], false) {
		return errRefused
	}
	for _, value := range v[3:7] {
		if _, ok := value.(uint64); !ok {
			return errRefused
		}
	}
	if _, ok := v[7].(uint32); !ok {
		return errRefused
	}
	for _, value := range v[8:] {
		if _, ok := value.(int32); !ok {
			return errRefused
		}
	}
	return nil
}
func (b *pauseBus) rawSnapshot(ctx context.Context) (pauseUnits, error) {
	var out pauseUnits
	t, err := b.api.GetUnitPropertiesContext(ctx, pauseTimer)
	if err != nil {
		return out, err
	}
	w, err := b.api.GetUnitPropertiesContext(ctx, pauseWorker)
	if err != nil {
		return out, err
	}
	out.TimerActive, out.TimerSub, out.TimerInvocation, out.TimerJob, err = pauseUnit(t, pauseTimer)
	if err != nil {
		return out, err
	}
	var workerJob uint32
	out.WorkerActive, out.WorkerSub, out.Invocation, workerJob, err = pauseUnit(w, pauseWorker)
	if err != nil {
		return out, err
	}
	if !pauseStrings(t["Triggers"], pauseWorker) || !pauseStrings(t["TriggeredBy"]) || !pauseStrings(w["Triggers"]) || !pauseStrings(w["TriggeredBy"], pauseTimer) {
		return out, errRefused
	}
	timer, err := b.api.GetUnitTypePropertiesContext(ctx, pauseTimer, "Timer")
	if err != nil || pauseTimerConfig(timer) != nil {
		return out, errRefused
	}
	service, err := b.api.GetUnitTypePropertiesContext(ctx, pauseWorker, "Service")
	if err != nil || pauseServiceConfig(service) != nil {
		return out, errRefused
	}
	main, m := service["MainPID"].(uint32)
	control, c := service["ControlPID"].(uint32)
	start, st := service["ExecMainStartTimestampMonotonic"].(uint64)
	result, re := service["Result"].(string)
	group, g := service["ControlGroup"].(string)
	if !m || !c || !st || !re || !g || group != "" && group != "/system.slice/"+pauseWorker {
		return out, errRefused
	}
	switch result {
	case "success", "exit-code", "signal", "core-dump", "timeout", "watchdog", "start-limit-hit", "resources", "protocol", "oom-kill", "exec-condition", "skipped":
	default:
		return out, errRefused
	}
	out.Main.PID, out.Control.PID, out.StartUSec, out.Result, out.Group = main, control, start, result, "/system.slice/"+pauseWorker
	if (main != 0 || control != 0) && (group == "" || out.Invocation == "" || start == 0) {
		return out, errRefused
	}
	// Only validated static configuration columns enter this digest. Runtime
	// timestamp/PID/result columns deliberately remain separate facts.
	config := []any{t["Id"], t["FragmentPath"], t["DropInPaths"], w["Id"], w["FragmentPath"], w["DropInPaths"], service["User"], service["Group"], service["Type"], service["Restart"], timer["Unit"]}
	raw, _ := json.Marshal(config)
	out.Configuration = fmt.Sprintf("%x", sha256.Sum256(raw))
	out.Jobs = []uint32{}
	if workerJob != 0 {
		out.Jobs = append(out.Jobs, workerJob)
	}
	jobs, err := b.api.ListJobsContext(ctx)
	if err != nil || len(jobs) > 10000 {
		return out, errRefused
	}
	seen := map[uint32]bool{}
	timerSeen, workerSeen := false, workerJob == 0
	for _, job := range jobs {
		if job.Id == 0 || seen[job.Id] || string(job.JobPath) != pauseJobPath(job.Id) {
			return out, errRefused
		}
		seen[job.Id] = true
		if job.Unit == pauseTimer {
			if job.Id != out.TimerJob {
				return out, errRefused
			}
			timerSeen = true
		}
		if job.Unit == pauseWorker {
			if job.JobType != "start" {
				return out, errRefused
			}
			if job.Id != workerJob {
				return out, errRefused
			}
			workerSeen = true
		}
	}
	if (out.TimerJob != 0) != timerSeen || !workerSeen {
		return out, errPauseBusy
	}
	return out, nil
}
func (b *pauseBus) snapshot(ctx context.Context, original *pauseUnits) (pauseUnits, error) {
	before, err := b.rawSnapshot(ctx)
	if original != nil && (err == nil || errors.Is(err, errPauseBusy)) && pauseUnitIdentity(*original, before) != nil {
		return before, errRefused
	}
	if err != nil {
		return before, err
	}
	if err = b.host.enrich(&before, original); err != nil {
		return before, err
	}
	after, err := b.rawSnapshot(ctx)
	if original != nil && (err == nil || errors.Is(err, errPauseBusy)) && pauseUnitIdentity(*original, after) != nil {
		return after, errRefused
	}
	if err != nil {
		return after, err
	}
	after.Main.Start, after.Control.Start = before.Main.Start, before.Control.Start
	after.GroupDevice, after.GroupInode = before.GroupDevice, before.GroupInode
	if !samePauseUnits(before, after) {
		return after, errPauseBusy
	}
	return before, nil
}
func (b *pauseBus) drained(ctx context.Context, before, current pauseUnits) error {
	return b.host.drained(ctx, before, current)
}
