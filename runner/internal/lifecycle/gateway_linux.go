package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"time"
)

func validServiceProcess(p serviceProcess) bool {
	if p.PID == 0 {
		return p == (serviceProcess{})
	}
	return p.PID <= 2147483647 && p.Start > 0 && p.UID > 0 && p.GID > 0 && p.ExecutableDevice > 0 && p.ExecutableInode > 0 && digestPattern.MatchString(p.ExecutableSHA)
}
func validController(v controllerFacts) bool {
	return pauseHex.MatchString(v.Invocation) && v.StartUSec > 0 && v.Main.PID > 0 && validServiceProcess(v.Main) && digestPattern.MatchString(v.Configuration)
}
func validGateway(v gatewayFacts) bool {
	if !digestPattern.MatchString(v.Configuration) || v.Group != gatewayGroup || (v.GroupDevice == 0) != (v.GroupInode == 0) || v.Jobs == nil || !validServiceProcess(v.Main) || !validServiceProcess(v.Control) || !knownServiceResult(v.Result) || v.Invocation != "" && !pauseHex.MatchString(v.Invocation) || v.Sub == "" {
		return false
	}
	switch v.Active {
	case "active", "activating", "deactivating", "inactive", "failed":
	default:
		return false
	}
	if (v.Main.PID != 0 || v.Control.PID != 0) && (v.GroupInode == 0 || v.Invocation == "" || v.StartUSec == 0) {
		return false
	}
	seen := map[uint32]bool{}
	for _, id := range v.Jobs {
		if id == 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func gatewayInitial(v gatewayFacts) bool {
	return validGateway(v) && v.Active == "active" && v.Sub == "running" && v.Main.PID > 0 && v.Control.PID == 0 && v.Job == 0 && len(v.Jobs) == 0
}
func gatewayUnitIdentity(a, b gatewayFacts) error {
	if a.Configuration != b.Configuration || a.StartUSec != b.StartUSec || b.Invocation != "" && a.Invocation != b.Invocation || b.Main.PID != 0 && a.Main.PID != b.Main.PID || b.Control.PID != 0 && a.Control.PID != b.Control.PID {
		return errRefused
	}
	return nil
}
func gatewayCompatible(a, b gatewayFacts) error {
	if !validGateway(b) || gatewayUnitIdentity(a, b) != nil || b.Main.PID != 0 && a.Main != b.Main || b.Control.PID != 0 && a.Control != b.Control || b.GroupDevice != 0 && (a.GroupDevice != b.GroupDevice || a.GroupInode != b.GroupInode) {
		return errRefused
	}
	return nil
}
func gatewayDrained(v gatewayFacts) bool {
	return validGateway(v) && (v.Active == "inactive" && v.Sub == "dead" || v.Active == "failed" && v.Sub == "failed") && v.Job == 0 && len(v.Jobs) == 0 && v.Main.PID == 0 && v.Control.PID == 0
}
func gatewayConflict(v gatewayFacts, job uint32) bool {
	if !digestPattern.MatchString(v.Configuration) {
		return false
	}
	if v.Active == "active" || v.Active == "activating" || v.Job != 0 && v.Job != job {
		return true
	}
	for _, id := range v.Jobs {
		if id != job {
			return true
		}
	}
	return false
}

type gatewayStopper struct {
	pause     pauser
	producer  string
	admission func(context.Context, receipt) (string, error)
	connect   func(context.Context) (gatewayConnection, error)
	interval  time.Duration
}
type gatewayOutcome struct {
	State         string `json:"state"`
	Code          string `json:"code,omitempty"`
	Job           uint32 `json:"gateway_job"`
	JobResult     string `json:"job_result,omitempty"`
	ServiceResult string `json:"service_result,omitempty"`
}

func (g gatewayStopper) execute(ctx context.Context, r receipt, executionSHA string) (gatewayOutcome, error) {
	out := gatewayOutcome{State: "unknown"}
	v, _ := g.pause.openPause(r, executionSHA)
	if v == nil {
		out.Code = "original_pause_unavailable"
		return out, errRefused
	}
	defer v.store.close()
	if v.observe(ctx).State != "watchdog_paused_worker_drained" {
		out.Code = "original_pause_unconfirmed"
		return out, errRefused
	}
	files, err := openServiceFiles(g.pause.files, g.pause.parent, "gateway-stop", "operation.lock", false)
	if err != nil {
		out.Code = "watchdog_operation_lock"
		return out, err
	}
	s := &gatewayStore{files}
	defer s.close()
	if v.observe(ctx).State != "watchdog_paused_worker_drained" {
		return out, errRefused
	}
	existing, err := s.existing()
	if err != nil {
		return out, err
	}
	b, err := g.connect(ctx)
	if err != nil {
		return out, err
	}
	defer b.close()
	same := func() error {
		boot, e := g.pause.boot()
		if e != nil || boot != v.saved.Intent.Boot {
			return errRefused
		}
		manager, e := b.identity(ctx)
		if e != nil || manager != v.saved.Intent.Manager || !s.unchanged() || !v.store.unchanged() {
			return errRefused
		}
		return nil
	}
	var saved gatewayRecords
	var controllerAfter controllerFacts
	windowVerified := false
	if existing {
		saved, err = s.load(r, executionSHA, g.producer, v)
		if err != nil || same() != nil {
			return out, errRefused
		}
	} else {
		if s.noWatchdogStop() != nil || same() != nil {
			out.Code = "watchdog_stop_owned"
			return out, errRefused
		}
		before, e := b.snapshot(ctx, nil)
		if e != nil || !gatewayInitial(before) {
			return out, errRefused
		}
		controller, e := b.controller(ctx)
		if e != nil || !validController(controller) {
			return out, errRefused
		}
		admission, e := g.admission(ctx, r)
		if e != nil || !digestPattern.MatchString(admission) {
			return out, errRefused
		}
		current, e := b.snapshot(ctx, &before)
		if e != nil || gatewayCompatible(before, current) != nil || !gatewayInitial(current) {
			return out, errRefused
		}
		c, e := b.controller(ctx)
		if e != nil || c != controller {
			return out, errRefused
		}
		again, e := g.admission(ctx, r)
		if e != nil || again != admission || v.observe(ctx).State != "watchdog_paused_worker_drained" || s.noWatchdogStop() != nil || same() != nil || ctx.Err() != nil {
			return out, errRefused
		}
		i := gatewayIntent{1, r.Expected.Operation, executionSHA, r.ProducerSHA, v.producer, v.saved.IntentSHA, bytesSHA(v.store.raw["drain.json"]), g.producer, v.saved.Intent.Boot, v.saved.Intent.Manager, gatewayUnit, admission, before, controller}
		if s.reserve() != nil || s.create("intent.json", i) != nil {
			return out, errRefused
		}
		saved, err = s.load(r, executionSHA, g.producer, v)
		if err != nil {
			return out, err
		}
		// The intent owns this one request, including any send-boundary failure.
		final, e := g.admission(ctx, r)
		if e != nil || final != admission {
			return out, errRefused
		}
		current, e = b.snapshot(ctx, &before)
		if e != nil || gatewayCompatible(before, current) != nil || !gatewayInitial(current) {
			return out, errRefused
		}
		c, e = b.controller(ctx)
		if e != nil || c != controller || v.observe(ctx).State != "watchdog_paused_worker_drained" || s.noWatchdogStop() != nil || same() != nil || ctx.Err() != nil {
			return out, errRefused
		}
		job, e := b.stopGateway(ctx)
		if e != nil || job.ID == 0 || job.Path != pauseJobPath(job.ID) {
			return out, errRefused
		}
		out.Job = job.ID
		job.IntentSHA = saved.IntentSHA
		if err = s.create("job.json", job); err != nil {
			return out, err
		}
		saved.Job = &job
		controllerAfter, e = b.controller(ctx)
		windowVerified = e == nil && controllerAfter == controller
		if !windowVerified {
			out.Code = "controller_window_unknown"
		}
	}
	if saved.Job == nil {
		out.Code = "gateway_job_unknown"
		return out, errRefused
	}
	out.Job = saved.Job.ID
	if saved.Event == nil {
		event, e := b.event(ctx, *saved.Job)
		if e != nil {
			return out, e
		}
		event.IntentSHA = saved.IntentSHA
		if same() != nil || event.ID != saved.Job.ID || event.Path != saved.Job.Path || event.Unit != gatewayUnit || event.Sender != managerOwner(saved.Intent.Manager) || !pauseJobResult(event.Result) {
			return out, errRefused
		}
		out.JobResult = event.Result
		if event.Result != "done" {
			out.State = "gateway_job_failed"
		}
		if err = s.create("event.json", event); err != nil {
			return out, err
		}
		saved.Event = &event
	}
	out.JobResult = saved.Event.Result
	if out.JobResult != "done" {
		out.State = "gateway_job_failed"
		return out, errRefused
	}
	if saved.Drain == nil && !windowVerified {
		if out.Code == "controller_window_unknown" {
			return out, errRefused
		}
		// With no durable window evidence, only the still-original live process
		// can supply it. A later controller stop cannot recreate this history.
		controllerAfter, err = b.controller(ctx)
		if err != nil || controllerAfter != saved.Intent.Controller {
			out.Code = "controller_window_unknown"
			return out, errRefused
		}
	}
	interval := g.interval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	wait := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			return nil
		}
	}
	for {
		if same() != nil {
			return out, errRefused
		}
		current, e := b.snapshot(ctx, &saved.Intent.Before)
		if gatewayConflict(current, saved.Job.ID) {
			return out, errRefused
		}
		if errors.Is(e, errPauseBusy) {
			if err = wait(); err != nil {
				return out, err
			}
			continue
		}
		if e != nil || gatewayCompatible(saved.Intent.Before, current) != nil {
			return out, errRefused
		}
		if gatewayDrained(current) {
			e = b.drained(ctx, saved.Intent.Before, current)
			if errors.Is(e, errPauseBusy) {
				if err = wait(); err != nil {
					return out, err
				}
				continue
			}
			if e != nil {
				return out, e
			}
			again, e := b.snapshot(ctx, &saved.Intent.Before)
			if gatewayConflict(again, saved.Job.ID) {
				return out, errRefused
			}
			if errors.Is(e, errPauseBusy) || e == nil && !reflect.DeepEqual(current, again) && gatewayCompatible(saved.Intent.Before, again) == nil {
				if err = wait(); err != nil {
					return out, err
				}
				continue
			}
			if e != nil || !reflect.DeepEqual(current, again) || same() != nil || ctx.Err() != nil || v.observe(ctx).State != "watchdog_paused_worker_drained" {
				return out, errRefused
			}
			out.ServiceResult = current.Result
			if saved.Drain == nil {
				if err = s.create("drain.json", gatewayDrain{saved.IntentSHA, saved.Job.ID, current, controllerAfter}); err != nil {
					return out, err
				}
			}
			if _, err = s.load(r, executionSHA, g.producer, v); err != nil {
				return out, err
			}
			out.State = "gateway_stopped_drained"
			out.Code = ""
			return out, nil
		}
		if err = wait(); err != nil {
			return out, err
		}
	}
}
