package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/headironc/designs/runner/internal/provider"
)

type pauseFixtureBus struct {
	mu                         sync.Mutex
	before                     pauseUnits
	manager, mode              string
	stopped                    bool
	sends                      int
	reads                      int
	busy                       int
	onSnapshot                 func()
	drainBusy                  int
	eventEntered, drainEntered chan struct{}
}

func (b *pauseFixtureBus) close() {}
func (b *pauseFixtureBus) identity(context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.manager, nil
}
func (b *pauseFixtureBus) snapshot(context.Context) (pauseUnits, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	if b.onSnapshot != nil {
		b.onSnapshot()
	}
	if b.stopped && b.busy > 0 {
		b.busy--
		return pauseUnits{}, errPauseBusy
	}
	v := b.before
	v.Jobs = append([]uint32{}, v.Jobs...)
	if b.stopped {
		v.TimerActive, v.TimerSub = "inactive", "dead"
		v.WorkerActive, v.WorkerSub = "inactive", "dead"
		v.Main, v.Control = pausePID{}, pausePID{}
	}
	if b.mode == "reactivated" && b.stopped {
		v.TimerActive = "active"
	}
	if b.mode == "replacement" && b.stopped {
		v.Invocation = strings.Repeat("9", 32)
	}
	return v, nil
}
func (b *pauseFixtureBus) stopTimer(context.Context) (pauseJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sends++
	b.stopped = true
	if b.mode == "lost_return" {
		return pauseJob{}, errRefused
	}
	return pauseJob{ID: 42, Path: pauseJobPath(42)}, nil
}
func (b *pauseFixtureBus) event(ctx context.Context, j pauseJob) (pauseEvent, error) {
	b.mu.Lock()
	mode, manager := b.mode, b.manager
	b.mu.Unlock()
	select {
	case b.eventEntered <- struct{}{}:
	default:
	}
	if mode == "missing_event" {
		<-ctx.Done()
		return pauseEvent{}, ctx.Err()
	}
	result := "done"
	if mode == "failed" {
		result = "failed"
	}
	return pauseEvent{ID: j.ID, Path: j.Path, Unit: pauseTimer, Sender: managerOwner(manager), Result: result}, nil
}
func (b *pauseFixtureBus) drained(context.Context, pauseUnits, pauseUnits) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case b.drainEntered <- struct{}{}:
	default:
	}
	if b.drainBusy > 0 {
		b.drainBusy--
		return errPauseBusy
	}
	return nil
}

func pauseFixture(t *testing.T) (pauser, *pauseFixtureBus, receipt, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "watchdog")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	c, path, operations := captureFixture(t)
	if _, err := c.execute(context.Background(), path, operations); err != nil {
		t.Fatal(err)
	}
	r, _ := parseRequest([]byte(requestJSON))
	var original receipt
	var digest string
	if ok, err := c.files.originalExecution(operations, r, []byte(requestJSON), strings.Repeat("6", 64), &original, &digest); !ok || err != nil {
		t.Fatal(err)
	}
	originals := map[string]string{}
	for _, name := range []string{"request.json", "execution.json", "complete.json"} {
		raw, err := os.ReadFile(filepath.Join(operations, r.Operation, name))
		if err != nil {
			t.Fatal(err)
		}
		originals[name] = string(raw)
	}
	t.Cleanup(func() {
		entries, err := os.ReadDir(filepath.Join(operations, r.Operation))
		if err != nil || len(entries) != 3 {
			t.Error("capture seal file set changed")
		}
		for name, expected := range originals {
			raw, err := os.ReadFile(filepath.Join(operations, r.Operation, name))
			if err != nil || string(raw) != expected {
				t.Error("capture original changed", name)
			}
		}
	})
	bus := &pauseFixtureBus{eventEntered: make(chan struct{}, 1), drainEntered: make(chan struct{}, 1), manager: strings.Repeat("a", 32) + "/:1.0", before: pauseUnits{TimerActive: "active", TimerSub: "waiting", TimerInvocation: strings.Repeat("b", 32), WorkerActive: "inactive", WorkerSub: "dead", Invocation: strings.Repeat("c", 32), StartUSec: 111, Result: "success", Group: "/system.slice/" + pauseWorker, Jobs: []uint32{}, Configuration: strings.Repeat("d", 64)}}
	p := pauser{files: protectedFiles{uid: uint32(os.Getuid()), boundary: root}, parent: parent, producer: strings.Repeat("7", 64), connect: func(context.Context) (pauseConnection, error) { return bus, nil }, boot: func() (string, error) { return bootUUID, nil }, admission: func(context.Context, receipt) (string, error) { return strings.Repeat("8", 64), nil }, interval: time.Millisecond}
	return p, bus, original, digest
}

func TestPauseOriginalResultAndReadOnlyConsumer(t *testing.T) {
	p, b, r, sha := pauseFixture(t)
	out, err := p.execute(context.Background(), r, sha)
	if err != nil || out.State != "watchdog_paused_worker_drained" || out.TimerJob != 42 || out.JobResult != "done" {
		t.Fatalf("%+v %v", out, err)
	}
	before := map[string]string{}
	for _, name := range []string{"intent.json", "job.json", "event.json", "drain.json"} {
		raw, err := os.ReadFile(filepath.Join(p.parent, "pause", name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = string(raw)
	}
	p.admission = func(context.Context, receipt) (string, error) {
		t.Fatal("recovery used API credentials")
		return "", errRefused
	}
	if _, err := p.execute(context.Background(), r, sha); err != nil {
		t.Fatal(err)
	}
	if got := p.observe(context.Background(), r, sha); got.State != "watchdog_paused_worker_drained" || got.JobResult != "done" {
		t.Fatalf("%+v", got)
	}
	if b.sends != 1 {
		t.Fatalf("resent %d times", b.sends)
	}
	for name, raw := range before {
		got, _ := os.ReadFile(filepath.Join(p.parent, "pause", name))
		if string(got) != raw {
			t.Fatal("immutable record changed")
		}
	}
	p.producer = strings.Repeat("9", 64)
	if got := p.observe(context.Background(), r, sha); got.State != "unknown" {
		t.Fatal("untrusted producer accepted")
	}
	if _, err := p.execute(context.Background(), r, sha); err == nil {
		t.Fatal("another binary resumed original")
	}
}

func TestPauseUnknownAndFailureNeverResend(t *testing.T) {
	for _, mode := range []string{"lost_return", "missing_event", "failed", "replacement", "reactivated"} {
		t.Run(mode, func(t *testing.T) {
			p, b, r, sha := pauseFixture(t)
			b.mode = mode
			run := func() (pauseOutcome, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if mode != "missing_event" {
					return p.execute(ctx, r, sha)
				}
				type result struct {
					o pauseOutcome
					e error
				}
				done := make(chan result, 1)
				go func() { o, e := p.execute(ctx, r, sha); done <- result{o, e} }()
				select {
				case <-b.eventEntered:
					cancel()
				case <-ctx.Done():
					t.Fatal("event stage not reached")
				}
				got := <-done
				return got.o, got.e
			}
			out, err := run()
			if err == nil || out.State == "watchdog_paused_worker_drained" {
				t.Fatal("unknown/failed completed")
			}
			if mode == "failed" && (out.State != "timer_job_failed" || out.JobResult != "failed") {
				t.Fatalf("lost known failure %+v", out)
			}
			p.admission = func(context.Context, receipt) (string, error) { t.Fatal("recovery called API"); return "", errRefused }
			run()
			if b.sends != 1 {
				t.Fatal("original request resent")
			}
		})
	}
}

func TestPauseBusyDrainContinuesOriginalObservation(t *testing.T) {
	p, b, r, sha := pauseFixture(t)
	b.busy = 2
	b.drainBusy = 3
	b.before.Result = "exit-code"
	out, err := p.execute(context.Background(), r, sha)
	if err != nil || out.WorkerResult != "exit-code" || b.sends != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	p2, b2, r2, sha2 := pauseFixture(t)
	b2.drainBusy = 100000
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan error, 1)
	go func() { _, err := p2.execute(ctx, r2, sha2); done <- err }()
	select {
	case <-b2.drainEntered:
		cancel()
	case <-ctx.Done():
		t.Fatal("drain stage not reached")
	}
	err = <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait not canceled: %v", err)
	}
	b2.drainBusy = 0
	p2.admission = func(context.Context, receipt) (string, error) {
		t.Fatal("recovery reacquired credentials")
		return "", errRefused
	}
	if _, err := p2.execute(context.Background(), r2, sha2); err != nil || b2.sends != 1 {
		t.Fatalf("natural drain recovery: %v", err)
	}
}

func TestPauseImmutableFailureBoundaries(t *testing.T) {
	for _, name := range []string{"intent.json", "job.json", "event.json", "drain.json"} {
		t.Run(name, func(t *testing.T) {
			p, b, r, sha := pauseFixture(t)
			failed := false
			p.files.syncHook = func(f *os.File) error {
				if !failed && filepath.Base(f.Name()) == name {
					failed = true
					return errRefused
				}
				return f.Sync()
			}
			if _, err := p.execute(context.Background(), r, sha); err == nil || !failed {
				t.Fatal("sync failure not retained")
			}
			p.files.syncHook = nil
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, recoveryErr := p.execute(ctx, r, sha)
			if (recoveryErr == nil) != (name != "intent.json") {
				t.Fatalf("durability recovery for %s: %v", name, recoveryErr)
			}
			want := 1
			if name == "intent.json" {
				want = 0
			}
			if b.sends != want {
				t.Fatalf("recovery resent: %d", b.sends)
			}
		})
	}
	for _, mode := range []string{"empty", "partial", "symlink", "hardlink", "lock_missing", "request", "boot", "manager", "producer", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			p, b, r, sha := pauseFixture(t)
			if mode == "empty" || mode == "partial" {
				os.Mkdir(filepath.Join(p.parent, "pause"), 0700)
				testWrite(t, filepath.Join(p.parent, "pause.lock"), nil, 0600)
				if mode == "partial" {
					testWrite(t, filepath.Join(p.parent, "pause", "intent.json"), []byte(`{"version":1`), 0600)
				}
			} else {
				if _, err := p.execute(context.Background(), r, sha); err != nil {
					t.Fatal(err)
				}
			}
			intent := filepath.Join(p.parent, "pause", "intent.json")
			switch mode {
			case "symlink":
				os.Rename(intent, intent+"-old")
				os.Symlink(intent+"-old", intent)
			case "hardlink":
				os.Link(intent, filepath.Join(p.parent, "link"))
			case "lock_missing":
				os.Remove(filepath.Join(p.parent, "pause.lock"))
			case "request":
				sha = strings.Repeat("e", 64)
			case "boot":
				p.boot = func() (string, error) { return vmUUID, nil }
			case "manager":
				b.manager = strings.Repeat("f", 32) + "/:1.2"
			case "producer":
				p.producer = strings.Repeat("9", 64)
			case "replacement":
				b.mode = "replacement"
			}
			prior := b.sends
			if _, err := p.execute(context.Background(), r, sha); err == nil {
				t.Fatal("changed/partial original accepted")
			}
			if b.sends != prior {
				t.Fatal("changed original resent")
			}
		})
	}
}

func TestPauseConcurrentCallerAndSendBoundary(t *testing.T) {
	p, b, r, sha := pauseFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	p.admission = func(context.Context, receipt) (string, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return strings.Repeat("8", 64), nil
	}
	done := make(chan error, 1)
	go func() { _, err := p.execute(context.Background(), r, sha); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("admission not entered")
	}
	if _, err := p.execute(context.Background(), r, sha); err == nil {
		t.Fatal("concurrent caller acquired lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b.sends != 1 {
		t.Fatal("concurrent send")
	}
	p2, b2, r2, sha2 := pauseFixture(t)
	var count int
	p2.admission = func(context.Context, receipt) (string, error) {
		count++
		if count == 3 {
			return strings.Repeat("9", 64), nil
		}
		return strings.Repeat("8", 64), nil
	}
	if _, err := p2.execute(context.Background(), r2, sha2); err == nil || b2.sends != 0 {
		t.Fatal("changed send-boundary admission accepted")
	}
	if _, err := p2.execute(context.Background(), r2, sha2); err == nil || b2.sends != 0 {
		t.Fatal("unsent durable intent retried")
	}
}

func TestPauseAdmissionSDKAllGlobalSources(t *testing.T) {
	_, _, r, _ := pauseFixture(t)
	for _, mode := range []string{"valid", "pool_enabled", "scale_enabled", "missing", "null_enabled", "duplicate", "conflict", "other_scope", "nul_fields", "missing_provider", "pool_null"} {
		t.Run(mode, func(t *testing.T) {
			bodies := diagnosticBodies()
			bodies["/repos/example/consumer/actions/jobs/9007199254740997"] = strings.Replace(bodies["/repos/example/consumer/actions/jobs/9007199254740997"], `"status":"in_progress"`, `"status":"completed","conclusion":"failure"`, 1)
			scale := bodies["/api/v1/scalesets/27"]
			extra := strings.Replace(scale, `"id":27`, `"id":28`, 1)
			pool := `{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","provider_name":"other-provider","org_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","endpoint":{"endpoint_type":"github","base_url":"https://github.com","api_base_url":"https://api.github.com"}}`
			bodies["/api/v1/pools"] = "[" + pool + "]"
			bodies["/api/v1/scalesets"] = "[" + scale + "]"
			switch mode {
			case "pool_enabled":
				bodies["/api/v1/pools"] = strings.Replace(bodies["/api/v1/pools"], `"provider_name":`, `"enabled":true,"provider_name":`, 1)
			case "scale_enabled":
				bodies["/api/v1/scalesets"] = strings.Replace(bodies["/api/v1/scalesets"], `"id":27`, `"enabled":true,"id":27`, 1)
			case "missing":
				bodies["/api/v1/scalesets"] = "[]"
			case "null_enabled":
				bodies["/api/v1/pools"] = strings.Replace(bodies["/api/v1/pools"], `"provider_name":`, `"enabled":null,"provider_name":`, 1)
			case "duplicate":
				bodies["/api/v1/scalesets"] = "[" + scale + "," + scale + "]"
			case "conflict":
				bodies["/api/v1/scalesets"] = "[" + scale + "," + extra + "]"
			case "other_scope":
				extra = strings.Replace(extra, r.Expected.GARMRepository, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 1)
				bodies["/api/v1/scalesets"] = "[" + scale + "," + extra + "]"
			case "nul_fields":
				pool = strings.Replace(pool, "other-provider", `other\u0000provider`, 1)
				bodies["/api/v1/pools"] = "[" + pool + "]"
			case "missing_provider":
				bodies["/api/v1/pools"] = `[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}]`
			case "pool_null":
				bodies["/api/v1/pools"] = "null"
			}
			got, err := platformFixture(t, bodies).pauseAdmission(context.Background(), r)
			allowed := mode == "valid" || mode == "other_scope" || mode == "nul_fields"
			if (err == nil) != allowed || err == nil && !digestPattern.MatchString(got) {
				t.Fatalf("mode=%s digest=%q err=%v", mode, got, err)
			}
		})
	}
}

func TestPauseDiagnosticKeepsClosureUnknown(t *testing.T) {
	p, _, r, sha := pauseFixture(t)
	if _, err := p.execute(context.Background(), r, sha); err != nil {
		t.Fatal(err)
	}
	d := diagnostician{job: func(context.Context, receipt) jobObservation {
		return jobObservation{State: "terminal", Conclusion: "failure"}
	}, registration: func(context.Context, receipt) resourceObservation { return resource("absent_verified", 0) }, garm: func(context.Context, receipt) garmObservation {
		return garmObservation{resourceObservation: resource("absent_verified", 0)}
	}, machine: func(context.Context, receipt) machineObservation { return machineObservation{} }, host: func(context.Context, receipt) hostObservation { return hostObservation{} }, guard: func(context.Context) (provider.GuardObservation, error) {
		return provider.GuardObservation{}, errRefused
	}, services: func(context.Context) []serviceObservation { return []serviceObservation{} }, database: func(context.Context, receipt) databaseObservation { return databaseObservation{} }, pause: func(ctx context.Context) pauseOutcome { return p.observe(ctx, r, sha) }}
	report := d.observe(context.Background(), r, strings.Repeat("a", 64))
	if report.WatchdogPause.State != "watchdog_paused_worker_drained" || report.Closure != "unknown" || report.Job.Conclusion != "failure" {
		t.Fatalf("narrow result upgraded outcome: %+v", report)
	}
	want := []string{"original_service_jobs_missing", "execution_credentials_closure_missing", "external_operation_receipts_missing", "original_controller_configuration_provenance_missing"}
	raw, _ := json.Marshal(report.ProvenanceGaps)
	expected, _ := json.Marshal(want)
	if string(raw) != string(expected) {
		t.Fatalf("lost independent gaps %s", raw)
	}
}

func TestPauseRecordRequiredFieldsAndInitialFacts(t *testing.T) {
	p, _, r, sha := pauseFixture(t)
	if _, err := p.execute(context.Background(), r, sha); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.parent, "pause", "intent.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var valid pauseIntent
	if decodePauseRecord(raw, &valid) != nil {
		t.Fatal("producer record refused")
	}
	for _, mode := range []string{"missing_zero", "null_zero", "null_nested", "timer_inactive", "timer_job", "timer_sub", "worker_stopping", "bad_guid"} {
		t.Run(mode, func(t *testing.T) {
			var doc map[string]any
			if json.Unmarshal(raw, &doc) != nil {
				t.Fatal("fixture")
			}
			before := doc["before"].(map[string]any)
			switch mode {
			case "missing_zero":
				delete(before, "timer_job")
			case "null_zero":
				before["timer_job"] = nil
			case "null_nested":
				before["main"] = nil
			case "timer_inactive":
				before["timer_active"] = "inactive"
			case "timer_job":
				before["timer_job"] = 7
			case "timer_sub":
				before["timer_sub"] = "unknown"
			case "worker_stopping":
				before["worker_active"], before["worker_sub"] = "deactivating", "stop"
			case "bad_guid":
				doc["manager"] = strings.Repeat("z", 32) + "/:1.0"
			}
			changed, _ := json.Marshal(doc)
			var record pauseIntent
			if decodePauseRecord(changed, &record) == nil && pauseInitialShape(record.Before) && managerOwner(record.Manager) != "" {
				t.Fatal("incomplete/invalid evidence accepted")
			}
		})
	}
}

func TestPauseAdmissionFingerprintStringBoundaries(t *testing.T) {
	_, _, r, _ := pauseFixture(t)
	var digests []string
	for _, pair := range [][2]string{{"left\x00middle", "right"}, {"left", "middle\x00right"}} {
		bodies := diagnosticBodies()
		job := "/repos/example/consumer/actions/jobs/9007199254740997"
		bodies[job] = strings.Replace(bodies[job], `"status":"in_progress"`, `"status":"completed","conclusion":"failure"`, 1)
		scale := bodies["/api/v1/scalesets/27"]
		var extra map[string]any
		json.Unmarshal([]byte(scale), &extra)
		extra["id"], extra["repo_id"], extra["name"], extra["provider_name"] = 28, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", pair[0], pair[1]
		raw, _ := json.Marshal(extra)
		bodies["/api/v1/scalesets"], bodies["/api/v1/pools"] = "["+scale+","+string(raw)+"]", "[]"
		digest, err := platformFixture(t, bodies).pauseAdmission(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, digest)
	}
	if digests[0] == digests[1] {
		t.Fatal("ambiguous string fingerprint")
	}
}

func TestPauseEntrypointRejectsAlternativeStateAndRedactsArguments(t *testing.T) {
	var output, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"pause-watchdog", "--pause-state", "/fixture-sensitive"}, &output, &diagnostic)
	if code != 1 || output.Len() != 0 || diagnostic.String() != "{\"event\":\"watchdog_pause_refused\",\"stage\":\"arguments\"}\n" {
		t.Fatalf("entrypoint result %d %q %q", code, output.String(), diagnostic.String())
	}
}
