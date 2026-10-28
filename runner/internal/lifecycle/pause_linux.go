package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

var pauseOwner = regexp.MustCompile(`^:[0-9]+\.[0-9]+$`)
var pauseHex = regexp.MustCompile(`^[0-9a-f]{32}$`)

func managerOwner(manager string) string {
	parts := strings.Split(manager, "/")
	if len(parts) != 2 || !pauseHex.MatchString(parts[0]) || !pauseOwner.MatchString(parts[1]) {
		return ""
	}
	return parts[1]
}
func pauseJobPath(id uint32) string { return fmt.Sprintf("/org/freedesktop/systemd1/job/%d", id) }
func pauseJobResult(v string) bool {
	switch v {
	case "done", "canceled", "timeout", "failed", "dependency", "skipped", "invalid", "assert", "unsupported", "collected", "once":
		return true
	}
	return false
}
func validPauseUnits(v pauseUnits) bool {
	if !digestPattern.MatchString(v.Configuration) || v.Group != "/system.slice/"+pauseWorker || v.Jobs == nil {
		return false
	}
	for _, id := range []string{v.Invocation, v.TimerInvocation} {
		if id != "" && !pauseHex.MatchString(id) {
			return false
		}
	}
	for _, p := range []pausePID{v.Main, v.Control} {
		if p.PID == 0 && p.Start != 0 || p.PID != 0 && p.Start == 0 {
			return false
		}
	}
	if (v.Main.PID != 0 || v.Control.PID != 0) && v.GroupInode == 0 {
		return false
	}
	switch v.WorkerActive {
	case "active", "activating", "inactive", "failed", "deactivating":
	default:
		return false
	}
	if v.WorkerSub == "" || v.TimerSub == "" {
		return false
	}
	seen := map[uint32]bool{}
	for _, id := range v.Jobs {
		if id == 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return v.Result != "" && (v.GroupInode == 0) == (v.GroupDevice == 0)
}
func pauseCompatible(before, current pauseUnits) error {
	if !validPauseUnits(current) || pauseUnitIdentity(before, current) != nil || current.GroupDevice != 0 && (current.GroupDevice != before.GroupDevice || current.GroupInode != before.GroupInode) {
		return errRefused
	}
	for _, pair := range [][2]pausePID{{before.Main, current.Main}, {before.Control, current.Control}} {
		if pair[1].PID != 0 && pair[0] != pair[1] {
			return errRefused
		}
	}
	return nil
}

// Check unit facts before a transient procfs read can end this observation.
func pauseUnitIdentity(before, current pauseUnits) error {
	if before.Configuration != current.Configuration || before.StartUSec != current.StartUSec || current.Invocation != "" && current.Invocation != before.Invocation || current.TimerInvocation != "" && current.TimerInvocation != before.TimerInvocation || current.Main.PID != 0 && current.Main.PID != before.Main.PID || current.Control.PID != 0 && current.Control.PID != before.Control.PID {
		return errRefused
	}
	for _, id := range current.Jobs {
		found := false
		for _, old := range before.Jobs {
			if id == old {
				found = true
			}
		}
		if !found {
			return errRefused
		}
	}
	return nil
}
func pauseDrainedShape(v pauseUnits) bool {
	return v.TimerActive == "inactive" && v.TimerSub == "dead" && v.TimerJob == 0 && (v.WorkerActive == "inactive" || v.WorkerActive == "failed") && (v.WorkerSub == "dead" || v.WorkerSub == "failed") && v.Main.PID == 0 && v.Control.PID == 0 && len(v.Jobs) == 0
}

func pauseTimerConflict(v pauseUnits, originalJob uint32) bool {
	return digestPattern.MatchString(v.Configuration) && (v.TimerJob != 0 && v.TimerJob != originalJob || v.TimerActive == "active" || v.TimerActive == "activating")
}

func pauseInitialShape(v pauseUnits) bool {
	if !validPauseUnits(v) || v.TimerActive != "active" || v.TimerJob != 0 {
		return false
	}
	switch v.TimerSub {
	case "waiting", "running", "elapsed":
	default:
		return false
	}
	return v.WorkerActive == "inactive" && v.WorkerSub == "dead" || v.WorkerActive == "failed" && v.WorkerSub == "failed" || v.WorkerActive == "activating" && v.WorkerSub == "start"
}

type pauseConnection interface {
	close()
	identity(context.Context) (string, error)
	snapshot(context.Context, *pauseUnits) (pauseUnits, error)
	stopTimer(context.Context) (pauseJob, error)
	event(context.Context, pauseJob) (pauseEvent, error)
	drained(context.Context, pauseUnits, pauseUnits) error
}
type pauser struct {
	files            protectedFiles
	parent, producer string
	admission        func(context.Context, receipt) (string, error)
	connect          func(context.Context) (pauseConnection, error)
	boot             func() (string, error)
	interval         time.Duration
}
type pauseOutcome struct {
	State        string `json:"state"`
	Code         string `json:"code,omitempty"`
	TimerJob     uint32 `json:"timer_job"`
	JobResult    string `json:"job_result,omitempty"`
	WorkerResult string `json:"worker_result,omitempty"`
}

func (p pauser) execute(ctx context.Context, r receipt, executionSHA string) (pauseOutcome, error) {
	out := pauseOutcome{State: "unknown"}
	s, err := openPauseStore(p.files, p.parent, true)
	if err != nil {
		return out, err
	}
	defer s.close()
	existing, err := s.existing()
	if err != nil {
		return out, err
	}
	b, err := p.connect(ctx)
	if err != nil {
		return out, err
	}
	defer b.close()
	same := func(i pauseIntent) error {
		boot, err := p.boot()
		if err != nil || boot != i.Boot {
			return errRefused
		}
		manager, err := b.identity(ctx)
		if err != nil || manager != i.Manager {
			return errRefused
		}
		return nil
	}
	var saved pauseRecords
	if existing {
		saved, err = s.load(r, executionSHA, p.producer)
		if err != nil {
			return out, err
		}
		if same(saved.Intent) != nil {
			return out, errRefused
		}
	} else {
		boot, err := p.boot()
		if err != nil || boot != r.Observed.Host.BootID {
			return out, errRefused
		}
		manager, err := b.identity(ctx)
		if err != nil || managerOwner(manager) == "" {
			return out, errRefused
		}
		before, err := b.snapshot(ctx, nil)
		if err != nil || !pauseInitialShape(before) {
			return out, errRefused
		}
		admission, err := p.admission(ctx, r)
		if err != nil {
			return out, err
		}
		current, err := b.snapshot(ctx, &before)
		if err != nil || pauseCompatible(before, current) != nil || current.TimerActive != "active" || current.TimerJob != 0 {
			return out, errRefused
		}
		repeated, err := p.admission(ctx, r)
		if err != nil || admission != repeated {
			return out, errRefused
		}
		i := pauseIntent{1, r.Expected.Operation, executionSHA, r.ProducerSHA, p.producer, boot, manager, pauseTimer, pauseWorker, admission, before}
		if same(i) != nil || ctx.Err() != nil || s.reserve() != nil {
			return out, errRefused
		}
		if err = s.create("intent.json", i); err != nil {
			return out, err
		}
		saved, err = s.load(r, executionSHA, p.producer)
		if err != nil {
			return out, err
		}
		// The durable intent owns the one request. Every later error is recovery,
		// including failure of the final observations before the call.
		finalAdmission, err := p.admission(ctx, r)
		if err != nil || finalAdmission != admission {
			return out, errRefused
		}
		current, err = b.snapshot(ctx, &before)
		if err != nil || pauseCompatible(before, current) != nil || current.TimerActive != "active" || current.TimerJob != 0 || same(i) != nil || ctx.Err() != nil || !s.unchanged() {
			return out, errRefused
		}
		job, err := b.stopTimer(ctx)
		if err != nil || job.ID == 0 || job.Path != pauseJobPath(job.ID) {
			return out, errRefused
		}
		out.TimerJob = job.ID
		job.IntentSHA = saved.IntentSHA
		if err = s.create("job.json", job); err != nil {
			return out, err
		}
		saved.Job = &job
	}
	if saved.Job == nil {
		return out, errRefused
	}
	out.TimerJob = saved.Job.ID
	if saved.Event == nil {
		event, err := b.event(ctx, *saved.Job)
		if err != nil {
			return out, err
		}
		event.IntentSHA = saved.IntentSHA
		if same(saved.Intent) != nil || event.ID != saved.Job.ID || event.Path != saved.Job.Path || event.Unit != pauseTimer || event.Sender != managerOwner(saved.Intent.Manager) || !pauseJobResult(event.Result) {
			return out, errRefused
		}
		out.JobResult = event.Result
		if event.Result != "done" {
			out.State = "timer_job_failed"
		}
		if err = s.create("event.json", event); err != nil {
			return out, err
		}
		saved.Event = &event
	}
	out.JobResult = saved.Event.Result
	if out.JobResult != "done" {
		out.State = "timer_job_failed"
		return out, errRefused
	}
	interval := p.interval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if same(saved.Intent) != nil || !s.unchanged() {
			return out, errRefused
		}
		current, err := b.snapshot(ctx, &saved.Intent.Before)
		// A complete unit observation remains evidence even when a later procfs
		// or job-list read finds a transient exit. Do not discard a known conflict.
		if pauseTimerConflict(current, saved.Job.ID) {
			return out, errRefused
		}
		if errors.Is(err, errPauseBusy) {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-tick.C:
			}
			continue
		}
		if err != nil {
			return out, err
		}
		if pauseCompatible(saved.Intent.Before, current) != nil {
			return out, errRefused
		}
		if pauseDrainedShape(current) {
			if err = b.drained(ctx, saved.Intent.Before, current); err != nil {
				if errors.Is(err, errPauseBusy) {
					select {
					case <-ctx.Done():
						return out, ctx.Err()
					case <-tick.C:
					}
					continue
				}
				return out, err
			}
			again, err := b.snapshot(ctx, &saved.Intent.Before)
			if pauseTimerConflict(again, saved.Job.ID) {
				return out, errRefused
			}
			if errors.Is(err, errPauseBusy) || err == nil && !samePauseUnits(current, again) && pauseCompatible(saved.Intent.Before, again) == nil {
				select {
				case <-ctx.Done():
					return out, ctx.Err()
				case <-tick.C:
				}
				continue
			}
			if err != nil || !samePauseUnits(current, again) || same(saved.Intent) != nil || ctx.Err() != nil {
				return out, errRefused
			}
			if saved.Drain == nil {
				out.WorkerResult = current.Result
				if err = s.create("drain.json", pauseDrain{saved.IntentSHA, saved.Job.ID, current}); err != nil {
					return out, err
				}
			}
			if _, err = s.load(r, executionSHA, p.producer); err != nil {
				return out, err
			}
			out.State = "watchdog_paused_worker_drained"
			out.WorkerResult = current.Result
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-tick.C:
		}
	}
}

func samePauseUnits(a, b pauseUnits) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}

func runPause(ctx context.Context, args []string, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"watchdog_pause_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	f := flag.NewFlagSet("pause-watchdog", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	requestPath := f.String("request", "", "original trusted request")
	operations := f.String("operations", "", "original operations directory")
	captureSHA := f.String("capture-sha256", "", "trusted original capture producer")
	gh := f.String("github-token-file", "", "existing credential for a first request")
	ga := f.String("garm-token-file", "", "existing credential for a first request")
	sessionSHA := f.String("garm-session-sha256", "", "trusted producer of a managed GARM session")
	if f.Parse(args) != nil || f.NArg() != 0 || *requestPath == "" || *operations == "" || !digestPattern.MatchString(*captureSHA) {
		return fail()
	}
	stage = "identity"
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return fail()
	}
	stage = "environment"
	if !controlledEnvironment(os.Environ()) {
		return fail()
	}
	stage = "secret_process"
	if protectSessionSecrets() != nil {
		return fail()
	}
	files := protectedFiles{uid: 0, boundary: "/"}
	stage = "original"
	raw, err := files.read(*requestPath, 0600, 64*1024)
	if err != nil {
		return fail()
	}
	r, err := parseRequest(raw)
	if err != nil {
		return fail()
	}
	var original receipt
	var executionSHA string
	ok, err := files.originalExecution(*operations, r, raw, *captureSHA, &original, &executionSHA)
	if err != nil || !ok {
		return fail()
	}
	producer, err := runningDigest()
	if err != nil {
		return fail()
	}
	host := pauseHost{proc: "/proc", cgroup: "/sys/fs/cgroup", realFS: true}
	p := pauser{files: files, parent: pauseParent, producer: producer, boot: host.boot, connect: func(c context.Context) (pauseConnection, error) {
		return connectPause(c, "/run/dbus/system_bus_socket", host)
	}}
	// Credential loading is lazy: existing original systemd requests can always
	// be observed without acquiring fresh API credentials.
	p.admission = sessionLiveAdmission(files, *gh, *ga, *sessionSHA)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stage = "original_pause"
	result, err := p.execute(ctx, original, executionSHA)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			result.Code = "pause_observation_expired"
		} else if errors.Is(err, context.Canceled) {
			result.Code = "pause_observation_cancelled"
		} else if result.Code == "" {
			result.Code = "pause_observation_refused"
		}
		_ = json.NewEncoder(diagnostic).Encode(struct {
			Event       string       `json:"event"`
			Stage       string       `json:"stage"`
			Observation pauseOutcome `json:"observation"`
		}{"watchdog_pause_refused", stage, result})
		return 1
	}
	stage = "output"
	data, _ := json.Marshal(result)
	data = append(data, '\n')
	if n, err := output.Write(data); err != nil || n != len(data) {
		return fail()
	}
	return 0
}
