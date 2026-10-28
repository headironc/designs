package lifecycle

import "context"

func (p pauser) observe(ctx context.Context, r receipt, executionSHA string) pauseOutcome {
	v, out := p.openPause(r, executionSHA)
	if v == nil {
		return out
	}
	defer v.store.close()
	return v.observe(ctx)
}

// pauseView retains the strict original files and pause.lock for the gateway's
// first real dependent action. Callers cannot replace a quiet-unit observation
// for this original job, nor release and reacquire its lock across that action.
type pauseView struct {
	pauser
	store *pauseStore
	saved pauseRecords
}

func (p pauser) openPause(r receipt, executionSHA string) (*pauseView, pauseOutcome) {
	out := pauseOutcome{State: "unknown", Code: "pause_record_unavailable"}
	if !digestPattern.MatchString(p.producer) {
		out.Code = "pause_producer_untrusted"
		return nil, out
	}
	s, err := openPauseStore(p.files, p.parent, false)
	if err != nil {
		return nil, out
	}
	fail := func() (*pauseView, pauseOutcome) { s.close(); return nil, out }
	exists, err := s.existing()
	if err != nil {
		return fail()
	}
	if !exists {
		out.Code = "pause_not_recorded"
		return fail()
	}
	saved, err := s.load(r, executionSHA, p.producer)
	if err != nil {
		return fail()
	}
	return &pauseView{p, s, saved}, out
}

func (v *pauseView) observe(ctx context.Context) pauseOutcome {
	p, s, saved := v.pauser, v.store, v.saved
	out := pauseOutcome{State: "unknown", Code: "pause_record_unavailable"}
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
	current, err := b.snapshot(ctx, &saved.Intent.Before)
	if err != nil || pauseCompatible(saved.Intent.Before, current) != nil || !pauseDrainedShape(current) || b.drained(ctx, saved.Intent.Before, current) != nil {
		return out
	}
	again, err := b.snapshot(ctx, &saved.Intent.Before)
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
