package lifecycle

import (
	"context"
	"reflect"
)

func (g gatewayStopper) observe(ctx context.Context, r receipt, executionSHA string) gatewayOutcome {
	out := gatewayOutcome{State: "unknown", Code: "gateway_record_unavailable"}
	if !digestPattern.MatchString(g.producer) {
		out.Code = "gateway_producer_untrusted"
		return out
	}
	v, _ := g.pause.openPause(r, executionSHA)
	if v == nil {
		out.Code = "original_pause_unavailable"
		return out
	}
	defer v.store.close()
	files, err := openServiceFiles(g.pause.files, g.pause.parent, "gateway-stop", "operation.lock", false)
	if err != nil {
		return out
	}
	s := &gatewayStore{files}
	defer s.close()
	exists, err := s.existing()
	if err != nil {
		return out
	}
	if !exists {
		out.Code = "gateway_not_recorded"
		return out
	}
	saved, err := s.load(r, executionSHA, g.producer, v)
	if err != nil {
		return out
	}
	if saved.Job == nil {
		out.Code = "gateway_job_unknown"
		return out
	}
	out.Job = saved.Job.ID
	if saved.Event == nil {
		out.Code = "gateway_event_unknown"
		return out
	}
	out.JobResult = saved.Event.Result
	b, err := g.connect(ctx)
	if err != nil {
		return out
	}
	defer b.close()
	same := func() bool {
		boot, e := g.pause.boot()
		if e != nil || boot != saved.Intent.Boot {
			return false
		}
		manager, e := b.identity(ctx)
		return e == nil && manager == saved.Intent.Manager && s.unchanged() && v.store.unchanged()
	}
	if !same() {
		return out
	}
	if out.JobResult != "done" {
		out.State = "gateway_job_failed"
		out.Code = ""
		return out
	}
	if saved.Drain == nil {
		out.Code = "gateway_drain_unrecorded"
		return out
	}
	// ControllerAfter was verified and sealed by the sender. An independently
	// authorized later controller stop cannot invalidate that original event.
	current, err := b.snapshot(ctx, &saved.Intent.Before)
	if err != nil || gatewayCompatible(saved.Intent.Before, current) != nil || !gatewayDrained(current) || b.drained(ctx, saved.Intent.Before, current) != nil {
		return out
	}
	again, err := b.snapshot(ctx, &saved.Intent.Before)
	if err != nil || !reflect.DeepEqual(current, again) || !same() {
		return out
	}
	out.State, out.Code, out.ServiceResult = "gateway_stopped_drained", "", current.Result
	return out
}
