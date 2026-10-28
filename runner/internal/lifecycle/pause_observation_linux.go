package lifecycle

import "context"

func (p pauser) observe(ctx context.Context, r receipt, executionSHA string) pauseOutcome {
	out := pauseOutcome{State: "unknown", Code: "pause_record_unavailable"}
	if !digestPattern.MatchString(p.producer) {
		out.Code = "pause_producer_untrusted"
		return out
	}
	s, err := openPauseStore(p.files, p.parent, false)
	if err != nil {
		return out
	}
	defer s.close()
	exists, err := s.existing()
	if err != nil {
		return out
	}
	if !exists {
		out.Code = "pause_not_recorded"
		return out
	}
	saved, err := s.load(r, executionSHA, p.producer)
	if err != nil {
		return out
	}
	if saved.Job == nil {
		out.Code = "pause_job_unknown"
		return out
	}
	out.TimerJob = saved.Job.ID
	if saved.Event == nil {
		out.Code = "pause_event_unknown"
		return out
	}
	out.JobResult = saved.Event.Result
	b, err := p.connect(ctx)
	if err != nil {
		return out
	}
	defer b.close()
	boot, err := p.boot()
	if err != nil || boot != saved.Intent.Boot {
		return out
	}
	manager, err := b.identity(ctx)
	if err != nil || manager != saved.Intent.Manager {
		return out
	}
	if out.JobResult != "done" {
		out.State = "timer_job_failed"
		out.Code = ""
		return out
	}
	if saved.Drain == nil {
		out.Code = "pause_drain_unrecorded"
		return out
	}
	current, err := b.snapshot(ctx)
	if err != nil || pauseCompatible(saved.Intent.Before, current) != nil || !pauseDrainedShape(current) || b.drained(ctx, saved.Intent.Before, current) != nil {
		return out
	}
	again, err := b.snapshot(ctx)
	if err != nil || !samePauseUnits(current, again) || !s.unchanged() {
		return out
	}
	boot, err = p.boot()
	if err != nil || boot != saved.Intent.Boot {
		return out
	}
	manager, err = b.identity(ctx)
	if err != nil || manager != saved.Intent.Manager {
		return out
	}
	out.State = "watchdog_paused_worker_drained"
	out.Code = ""
	out.WorkerResult = current.Result
	return out
}
