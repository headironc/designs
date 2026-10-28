package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type sessionWindowFixtureBus struct {
	boot, manager string
	watch         pauseUnits
	before, after gatewayFacts
	stopped       bool
	started       bool
	sends         []string
	lostReply     string
	lostEvent     string
	badEvent      string
	drainFailure  string
}

func (b *sessionWindowFixtureBus) close() {}

func (b *sessionWindowFixtureBus) listener(context.Context, serviceProcess) (sessionSocket, error) {
	inode := uint64(100)
	if b.started {
		inode = 101
	}
	return sessionSocket{Inode: inode, Namespace: 200, Device: 1}, nil
}

func (b *sessionWindowFixtureBus) identity(context.Context) (string, string, error) {
	return b.boot, b.manager, nil
}
func (b *sessionWindowFixtureBus) watchdog(context.Context, *pauseUnits) (pauseUnits, error) {
	return b.watch, nil
}
func (b *sessionWindowFixtureBus) controller(context.Context, *gatewayFacts) (gatewayFacts, error) {
	if b.started {
		return b.after, nil
	}
	v := b.before
	if b.stopped {
		v.Active, v.Sub = "inactive", "dead"
		v.Main = serviceProcess{}
		v.GroupDevice, v.GroupInode = 0, 0
	}
	return v, nil
}
func (b *sessionWindowFixtureBus) send(stage string, id uint32) (pauseJob, error) {
	b.sends = append(b.sends, stage)
	if b.lostReply == stage {
		return pauseJob{}, errRefused
	}
	return pauseJob{ID: id, Path: pauseJobPath(id)}, nil
}
func (b *sessionWindowFixtureBus) stopTimer(context.Context) (pauseJob, error) {
	b.watch.TimerActive, b.watch.TimerSub = "inactive", "dead"
	b.watch.WorkerActive, b.watch.WorkerSub = "inactive", "dead"
	b.watch.Main, b.watch.Control = pausePID{}, pausePID{}
	return b.send("watchdog", 41)
}
func (b *sessionWindowFixtureBus) stopController(context.Context) (pauseJob, error) {
	b.stopped = true
	return b.send("stop", 42)
}
func (b *sessionWindowFixtureBus) startController(context.Context) (pauseJob, error) {
	b.started = true
	return b.send("start", 43)
}
func (b *sessionWindowFixtureBus) event(_ context.Context, j pauseJob, unit string) (pauseEvent, error) {
	stage := map[uint32]string{41: "watchdog", 42: "stop", 43: "start"}[j.ID]
	if b.lostEvent == stage {
		return pauseEvent{}, errRefused
	}
	e := pauseEvent{ID: j.ID, Path: j.Path, Unit: unit, Sender: managerOwner(b.manager), Result: "done"}
	if b.badEvent == stage {
		e.Unit = "other.service"
	}
	return e, nil
}
func (b *sessionWindowFixtureBus) watchdogDrained(context.Context, pauseUnits, pauseUnits) error {
	if b.drainFailure == "watchdog" {
		return errRefused
	}
	return nil
}
func (b *sessionWindowFixtureBus) controllerDrained(context.Context, gatewayFacts, gatewayFacts) error {
	if b.drainFailure == "controller" {
		return errRefused
	}
	return nil
}

func sessionWindowFixture(t *testing.T) (sessionWindow, sessionPlan, []byte, *sessionWindowFixtureBus) {
	t.Helper()
	root := t.TempDir()
	parent := filepath.Join(root, "credentials")
	if os.Mkdir(parent, 0700) != nil {
		t.Fatal("fixture parent")
	}
	files := protectedFiles{uid: uint32(os.Getuid()), boundary: root}
	s, err := openServiceFiles(files, parent, "session", "session.lock", true)
	if err != nil {
		t.Fatal(err)
	}
	window, err := s.sibling("window")
	if err != nil {
		t.Fatal(err)
	}
	// Closing the original descriptor must not release the duplicate's lease.
	s.close()
	t.Cleanup(window.close)
	before, entropy := sessionConfigurationFixture(t)
	path := filepath.Join(root, "etc", "garm.toml")
	testWrite(t, path, before, 0640)
	p := sessionPlan{1, "aaaaaaaa-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002", "haloadmin", bootUUID, strings.Repeat("1", 32), strings.Repeat("b", 64), bytesSHA(before), 86400, 1863129900}
	raw, _ := json.Marshal(p)
	b := &sessionWindowFixtureBus{boot: p.Boot, manager: strings.Repeat("a", 32) + "/:1.0"}
	b.watch = pauseUnits{TimerActive: "active", TimerSub: "waiting", TimerInvocation: strings.Repeat("b", 32), WorkerActive: "inactive", WorkerSub: "dead", Invocation: strings.Repeat("c", 32), StartUSec: 111, Result: "success", Group: "/system.slice/" + pauseWorker, Jobs: []uint32{}, Configuration: strings.Repeat("d", 64)}
	b.before = gatewayFacts{Active: "active", Sub: "running", Invocation: p.Invocation, StartUSec: 100, Main: serviceProcess{PID: 123, Start: 9, UID: 24567, GID: 24568, ExecutableDevice: 1, ExecutableInode: 2, ExecutableSHA: p.BinarySHA}, Group: "/system.slice/" + controllerUnit, GroupDevice: 3, GroupInode: 4, Jobs: []uint32{}, Result: "success", Configuration: strings.Repeat("e", 64)}
	b.after = b.before
	b.after.Invocation, b.after.StartUSec, b.after.Main.PID, b.after.Main.Start = strings.Repeat("2", 32), 200, 124, 19
	w := sessionWindow{store: window, services: b, installer: sessionInstaller{path: path, gid: uint32(os.Getgid())}, producer: strings.Repeat("f", 64), entropy: bytes.NewReader(entropy), now: func() time.Time { return time.Unix(1863129600, 0) }, admission: func(context.Context) (string, error) { return strings.Repeat("8", 64), nil }, probe: func(context.Context, []byte) error { return nil }, domain: func(context.Context, []byte, []byte, gatewayFacts) (string, error) {
		return strings.Repeat("9", 64), nil
	}}
	w.startLease = func(action func() error) error { return action() }
	w.domain = func(_ context.Context, _, _ []byte, current gatewayFacts) (string, error) {
		proof := sessionDomainProof{ProbeSHA: strings.Repeat("9", 64), Old: p.Invocation, New: current.Invocation, Rejected: 401, Accepted: 200}
		a := sessionAcquirer{files: window.files}
		if a.absent(window, "old-domain.rejected.json") {
			if window.create("old-domain.rejected.json", proof) != nil {
				return "", errRefused
			}
		}
		var saved sessionDomainProof
		raw, err := a.read(window, "old-domain.rejected.json", &saved)
		if err != nil || saved != proof {
			return "", errRefused
		}
		return bytesSHA(raw), nil
	}
	return w, p, raw, b
}

func TestSessionWindowOriginalJobsAndLoadedConfiguration(t *testing.T) {
	w, p, raw, b := sessionWindowFixture(t)
	ready, err := w.execute(context.Background(), p, raw)
	if err != nil || ready.After.Invocation != b.after.Invocation || ready.ConfigSHA == p.ConfigSHA || !reflect.DeepEqual(b.sends, []string{"watchdog", "stop", "start"}) {
		t.Fatalf("window did not finish its exact sequence: %+v %v sends=%v", ready, err, b.sends)
	}
	w.entropy = nil
	w.startLease = func(func() error) error {
		t.Fatal("completed original start job released the provider lock again")
		return errRefused
	}
	again, err := w.execute(context.Background(), p, raw)
	if err != nil || !equalWindowReady(ready, again) || len(b.sends) != 3 {
		t.Fatalf("original recovery sent another action: %+v %v sends=%v", again, err, b.sends)
	}
	other := p
	other.Operation = "ffffffff-0000-4000-8000-000000000006"
	otherRaw, _ := json.Marshal(other)
	if _, err := w.execute(context.Background(), other, otherRaw); err == nil || len(b.sends) != 3 {
		t.Fatal("a new operation bypassed the original window")
	}
}

func TestSessionWindowLostJobReplyNeverResends(t *testing.T) {
	for _, stage := range []string{"watchdog", "stop", "start"} {
		t.Run(stage, func(t *testing.T) {
			w, p, raw, b := sessionWindowFixture(t)
			b.lostReply = stage
			if _, err := w.execute(context.Background(), p, raw); err == nil {
				t.Fatal("lost reply claimed success")
			}
			count := len(b.sends)
			b.lostReply = ""
			for range 2 {
				if _, err := w.execute(context.Background(), p, raw); err == nil || len(b.sends) != count {
					t.Fatal("recovery repeated an unknown service operation")
				}
			}
		})
	}
}

func TestSessionWindowLostEventMayOnlyObserveOriginalJob(t *testing.T) {
	for _, stage := range []string{"watchdog", "stop", "start"} {
		t.Run(stage, func(t *testing.T) {
			w, p, raw, b := sessionWindowFixture(t)
			b.lostEvent = stage
			if _, err := w.execute(context.Background(), p, raw); err == nil {
				t.Fatal("missing original event claimed success")
			}
			count := len(b.sends)
			if _, err := w.execute(context.Background(), p, raw); err == nil || len(b.sends) != count {
				t.Fatal("inactive state replaced a missing event or sent another job")
			}
			b.lostEvent = ""
			if _, err := w.execute(context.Background(), p, raw); err != nil || len(b.sends) != 3 {
				t.Fatalf("original event recovery failed: %v", err)
			}
		})
	}
}

func TestSessionWindowRefusesDrainFailureBeforeNextEffect(t *testing.T) {
	for _, stage := range []string{"watchdog", "controller"} {
		t.Run(stage, func(t *testing.T) {
			w, p, raw, b := sessionWindowFixture(t)
			before, _ := os.ReadFile(w.installer.path)
			b.drainFailure = stage
			if _, err := w.execute(context.Background(), p, raw); err == nil {
				t.Fatal("undrained original process admitted")
			}
			after, _ := os.ReadFile(w.installer.path)
			if !bytes.Equal(before, after) || b.started || stage == "watchdog" && b.stopped {
				t.Fatal("drain failure allowed the next effect")
			}
		})
	}
}

func TestSessionWindowFinalSealSyncFailureRecoversOriginal(t *testing.T) {
	w, p, raw, b := sessionWindowFixture(t)
	failed := false
	w.store.files.syncHook = func(f *os.File) error {
		if f.Name() == w.store.path && !failed {
			if _, err := os.Stat(filepath.Join(w.store.path, "window.ready.json")); err == nil {
				failed = true
				return errors.New("fixture fsync failure")
			}
		}
		return nil
	}
	if _, err := w.execute(context.Background(), p, raw); err == nil || !failed {
		t.Fatal("fixture did not interrupt final publication")
	}
	w.store.files.syncHook = nil
	if _, err := w.execute(context.Background(), p, raw); err != nil || len(b.sends) != 3 {
		t.Fatalf("lost seal response caused another action: %v %v", err, b.sends)
	}
}

func TestSessionWindowRefusesChangedRuntimeAndAdmission(t *testing.T) {
	for _, kind := range []string{"boot", "manager", "controller", "binary", "watchdog", "admission", "domain", "expired", "old_key"} {
		t.Run(kind, func(t *testing.T) {
			w, p, raw, b := sessionWindowFixture(t)
			if _, err := w.execute(context.Background(), p, raw); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "boot":
				b.boot = "ffffffff-0000-4000-8000-000000000006"
			case "manager":
				b.manager = strings.Repeat("b", 32) + "/:1.1"
			case "controller":
				b.after.Invocation = strings.Repeat("3", 32)
			case "binary":
				b.after.Main.ExecutableSHA = strings.Repeat("3", 64)
			case "watchdog":
				b.watch.TimerActive = "active"
			case "admission":
				w.admission = func(context.Context) (string, error) { return strings.Repeat("3", 64), nil }
			case "domain":
				w.domain = func(context.Context, []byte, []byte, gatewayFacts) (string, error) { return "", errRefused }
			case "expired":
				w.now = func() time.Time { return time.Unix(p.Expires, 0) }
			case "old_key":
				data, _ := os.ReadFile(filepath.Join(w.store.path, "configuration.before.toml"))
				testWrite(t, w.installer.path, data, 0640)
			}
			if _, err := w.execute(context.Background(), p, raw); err == nil || len(b.sends) != 3 {
				t.Fatal("changed window accepted or service action repeated")
			}
		})
	}
}
