package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"
)

// The window has exactly three service writes. The backend owns observation
// of the loaded units, original processes, cgroups and the subscribed bus.
type sessionWindowServices interface {
	close()
	identity(context.Context) (string, string, error)
	watchdog(context.Context, *pauseUnits) (pauseUnits, error)
	controller(context.Context, *gatewayFacts) (gatewayFacts, error)
	listener(context.Context, serviceProcess) (sessionSocket, error)
	stopTimer(context.Context) (pauseJob, error)
	stopController(context.Context) (pauseJob, error)
	startController(context.Context) (pauseJob, error)
	event(context.Context, pauseJob, string) (pauseEvent, error)
	watchdogDrained(context.Context, pauseUnits, pauseUnits) error
	controllerDrained(context.Context, gatewayFacts, gatewayFacts) error
}

type sessionWindowIntent struct {
	Version      int           `json:"version"`
	RequestSHA   string        `json:"request_sha256"`
	ProducerSHA  string        `json:"producer_sha256"`
	AdmissionSHA string        `json:"admission_sha256"`
	Boot         string        `json:"boot_id"`
	Manager      string        `json:"manager"`
	Watchdog     pauseUnits    `json:"watchdog"`
	Controller   gatewayFacts  `json:"controller"`
	Listener     sessionSocket `json:"listener"`
}

type sessionWindowReady struct {
	IntentSHA string        `json:"intent_sha256"`
	PausedSHA string        `json:"paused_sha256"`
	StopSHA   string        `json:"stopped_sha256"`
	ConfigSHA string        `json:"installed_configuration_sha256"`
	StartSHA  string        `json:"started_sha256"`
	DomainSHA string        `json:"old_domain_rejection_sha256"`
	After     gatewayFacts  `json:"after"`
	Listener  sessionSocket `json:"listener"`
}

type sessionWindowJob struct {
	WindowSHA string `json:"window_sha256"`
	BeforeSHA string `json:"before_sha256"`
	Unit      string `json:"unit"`
	Method    string `json:"method"`
}

type sessionWindow struct {
	store     *serviceFiles
	services  sessionWindowServices
	installer sessionInstaller
	producer  string
	now       func() time.Time
	entropy   io.Reader
	// Only the original controller start job may yield the provider lock for
	// ExecStartPre. Reacquisition must verify the unchanged original guard.
	startLease func(func() error) error
	// Admission includes original history/consumer locks and global resources;
	// it must be rechecked while all of those locks remain held.
	admission func(context.Context) (string, error)
	// This verifies the new authenticated server and rejects a valid old-key
	// probe. It does not claim recovery of an unrecorded historical token.
	probe  func(context.Context, []byte) error
	domain func(context.Context, []byte, []byte, gatewayFacts) (string, error)
}

func sessionControllerRunning(v gatewayFacts) bool {
	return v.Active == "active" && v.Sub == "running" && pauseHex.MatchString(v.Invocation) && v.StartUSec > 0 && v.Main.PID != 0 && v.Main.Start > 0 && v.Main.UID != 0 && v.Main.GID != 0 && digestPattern.MatchString(v.Main.ExecutableSHA) && v.Main.ExecutableDevice != 0 && v.Main.ExecutableInode != 0 && v.Control == (serviceProcess{}) && v.Group == "/system.slice/"+controllerUnit && v.GroupDevice != 0 && v.GroupInode != 0 && v.Job == 0 && v.Jobs != nil && len(v.Jobs) == 0 && digestPattern.MatchString(v.Configuration) && v.Result == "success"
}

func sessionControllerStopped(before, current gatewayFacts) bool {
	return sessionControllerRunning(before) && (current.Active == "inactive" || current.Active == "failed") && (current.Sub == "dead" || current.Sub == "failed") && current.Main == (serviceProcess{}) && current.Control == (serviceProcess{}) && (current.Invocation == "" || current.Invocation == before.Invocation) && current.StartUSec == before.StartUSec && current.Configuration == before.Configuration && current.Group == before.Group && (current.GroupDevice == 0 && current.GroupInode == 0 || current.GroupDevice == before.GroupDevice && current.GroupInode == before.GroupInode) && current.Job == 0 && current.Jobs != nil && len(current.Jobs) == 0 && knownServiceResult(current.Result)
}

func sessionControllerRestarted(before, current gatewayFacts) bool {
	return sessionControllerRunning(current) && current.Invocation != before.Invocation && current.StartUSec > before.StartUSec && current.Main.Start > before.Main.Start && current.Configuration == before.Configuration && current.Main.UID == before.Main.UID && current.Main.GID == before.Main.GID && current.Main.ExecutableSHA == before.Main.ExecutableSHA && current.Main.ExecutableDevice == before.Main.ExecutableDevice && current.Main.ExecutableInode == before.Main.ExecutableInode
}

// serviceJob never sends on recovery of an existing intent. If the original
// reply was lost, an inactive unit is not a replacement for that job receipt.
func (w sessionWindow) serviceJob(ctx context.Context, name, windowSHA, beforeSHA, unit, method string, check func() error, send func(context.Context) (pauseJob, error)) error {
	s := w.store
	a := sessionAcquirer{files: s.files}
	intent := sessionWindowJob{windowSHA, beforeSHA, unit, method}
	var saved sessionWindowJob
	newIntent := a.absent(s, name+".intent.json")
	if newIntent {
		if !a.absent(s, name+".job.json") || !a.absent(s, name+".event.json") || check() != nil || s.create(name+".intent.json", intent) != nil || check() != nil {
			return errRefused
		}
	} else if _, err := a.read(s, name+".intent.json", &saved); err != nil || saved != intent {
		return errRefused
	}
	intentSHA := bytesSHA(s.raw[name+".intent.json"])
	var job pauseJob
	if newIntent {
		result, err := send(ctx)
		if err != nil || result.ID == 0 || result.Path != pauseJobPath(result.ID) {
			return errRefused
		}
		result.IntentSHA = intentSHA
		if s.create(name+".job.json", result) != nil {
			return errRefused
		}
	}
	if _, err := a.read(s, name+".job.json", &job); err != nil || job.IntentSHA != intentSHA || job.ID == 0 || job.Path != pauseJobPath(job.ID) {
		return errRefused
	}
	var event pauseEvent
	if a.absent(s, name+".event.json") {
		result, err := w.services.event(ctx, job, unit)
		if err != nil {
			return errRefused
		}
		result.IntentSHA = intentSHA
		// Validate the complete original event before retaining acceptance.
		if !w.validEvent(result, job, intentSHA, unit) || s.create(name+".event.json", result) != nil {
			return errRefused
		}
	}
	if _, err := a.read(s, name+".event.json", &event); err != nil || !w.validEvent(event, job, intentSHA, unit) || event.Result != "done" || !s.unchanged() {
		return errRefused
	}
	return nil
}

func (w sessionWindow) validEvent(e pauseEvent, j pauseJob, intentSHA, unit string) bool {
	var original sessionWindowIntent
	return strictObject(w.store.raw["window.intent.json"], &original) == nil && e.IntentSHA == intentSHA && e.ID == j.ID && e.Path == j.Path && e.Unit == unit && e.Sender == managerOwner(original.Manager) && pauseJobResult(e.Result)
}

func (w sessionWindow) execute(ctx context.Context, p sessionPlan, request []byte) (sessionWindowReady, error) {
	out := sessionWindowReady{}
	s := w.store
	if s == nil || w.services == nil || w.now == nil || w.admission == nil || w.probe == nil || w.domain == nil || w.startLease == nil || !digestPattern.MatchString(w.producer) {
		return out, errRefused
	}
	decoded, err := decodeSessionPlan(request)
	if err != nil || decoded != p || !s.unchanged() {
		return out, errRefused
	}
	exists, err := s.existing()
	if err != nil {
		return out, errRefused
	}
	a := sessionAcquirer{files: s.files}
	var original sessionWindowIntent
	if exists {
		if _, err := a.read(s, "window.intent.json", &original); err != nil || original.Version != 1 || original.RequestSHA != bytesSHA(request) || original.ProducerSHA != w.producer || original.Boot != p.Boot || managerOwner(original.Manager) == "" || !digestPattern.MatchString(original.AdmissionSHA) || !validPauseUnits(original.Watchdog) || !sessionControllerRunning(original.Controller) || !original.Listener.valid() || original.Controller.Invocation != p.Invocation || original.Controller.Main.ExecutableSHA != p.BinarySHA {
			return out, errRefused
		}
	}
	check := func() error {
		if ctx.Err() != nil || w.now().Unix() >= p.Expires || !s.unchanged() {
			return errRefused
		}
		boot, manager, err := w.services.identity(ctx)
		if err != nil || boot != p.Boot || original.Manager != "" && manager != original.Manager || managerOwner(manager) == "" {
			return errRefused
		}
		admission, err := w.admission(ctx)
		if err != nil || !digestPattern.MatchString(admission) || original.AdmissionSHA != "" && admission != original.AdmissionSHA {
			return errRefused
		}
		return nil
	}
	if check() != nil {
		return out, errRefused
	}
	if !exists {
		watchdog, err := w.services.watchdog(ctx, nil)
		if err != nil || !validPauseUnits(watchdog) || !pauseInitialShape(watchdog) && !pauseDrainedShape(watchdog) {
			return out, errRefused
		}
		controller, err := w.services.controller(ctx, nil)
		if err != nil || !sessionControllerRunning(controller) || controller.Invocation != p.Invocation || controller.Main.ExecutableSHA != p.BinarySHA {
			return out, errRefused
		}
		listener, err := w.services.listener(ctx, controller.Main)
		if err != nil || !listener.valid() {
			return out, errRefused
		}
		boot, manager, err := w.services.identity(ctx)
		if err != nil || boot != p.Boot {
			return out, errRefused
		}
		admission, err := w.admission(ctx)
		if err != nil || !digestPattern.MatchString(admission) {
			return out, errRefused
		}
		original = sessionWindowIntent{1, bytesSHA(request), w.producer, admission, boot, manager, watchdog, controller, listener}
		if check() != nil || s.reserve() != nil || s.create("window.intent.json", original) != nil {
			return out, errRefused
		}
	}
	windowSHA := bytesSHA(s.raw["window.intent.json"])
	if w.serviceJob(ctx, "watchdog", windowSHA, windowSHA, pauseTimer, "StopUnit", check, w.services.stopTimer) != nil {
		return out, errRefused
	}
	paused := func() error {
		current, err := w.services.watchdog(ctx, &original.Watchdog)
		if err != nil || !pauseDrainedShape(current) || pauseCompatible(original.Watchdog, current) != nil || w.services.watchdogDrained(ctx, original.Watchdog, current) != nil || check() != nil {
			return errRefused
		}
		if a.absent(s, "watchdog.drained.json") {
			if s.create("watchdog.drained.json", current) != nil {
				return errRefused
			}
		}
		var saved pauseUnits
		if _, err := a.read(s, "watchdog.drained.json", &saved); err != nil || !samePauseUnits(saved, current) {
			return errRefused
		}
		return nil
	}
	if paused() != nil {
		return out, errRefused
	}
	w.installer.store = s
	if w.installer.prepare(p.ConfigSHA, w.entropy) != nil {
		return out, errRefused
	}
	_, oldConfiguration, _, err := w.installer.original()
	if err != nil || w.probe(ctx, oldConfiguration) != nil || paused() != nil {
		return out, errRefused
	}
	if w.serviceJob(ctx, "controller.stop", windowSHA, bytesSHA(s.raw["watchdog.drained.json"]), controllerUnit, "StopUnit", paused, w.services.stopController) != nil {
		return out, errRefused
	}
	stopped := func(ctx context.Context) error {
		if paused() != nil {
			return errRefused
		}
		current, err := w.services.controller(ctx, &original.Controller)
		if err != nil || !sessionControllerStopped(original.Controller, current) || w.services.controllerDrained(ctx, original.Controller, current) != nil || check() != nil {
			return errRefused
		}
		if a.absent(s, "controller.stopped.json") {
			if s.create("controller.stopped.json", current) != nil {
				return errRefused
			}
		}
		var saved gatewayFacts
		if _, err := a.read(s, "controller.stopped.json", &saved); err != nil || !sessionControllerStopped(original.Controller, saved) {
			return errRefused
		}
		return nil
	}
	w.installer.stopped = stopped
	// Once StartUnit has an intent, do not demand that the controller still be
	// stopped. The installer then only verifies its completed original exchange.
	if !a.absent(s, "controller.start.intent.json") {
		if a.absent(s, "configuration.installed.json") {
			return out, errRefused
		}
	} else if stopped(ctx) != nil {
		return out, errRefused
	}
	var stoppedOriginal gatewayFacts
	if _, err := a.read(s, "controller.stopped.json", &stoppedOriginal); err != nil || !sessionControllerStopped(original.Controller, stoppedOriginal) {
		return out, errRefused
	}
	if w.installer.apply(ctx) != nil {
		return out, errRefused
	}
	config, before, after, err := w.installer.original()
	if err != nil || config.BeforeSHA != p.ConfigSHA {
		return out, errRefused
	}
	startCheck := func() error {
		if check() != nil || paused() != nil || stopped(ctx) != nil {
			return errRefused
		}
		return nil
	}
	start := func() error {
		return w.serviceJob(ctx, "controller.start", windowSHA, bytesSHA(s.raw["configuration.installed.json"]), controllerUnit, "StartUnit", startCheck, w.services.startController)
	}
	// A retained completion event needs only readback; yielding again would
	// unnecessarily reopen provider admission during an original-result read.
	if a.absent(s, "controller.start.event.json") {
		err = w.startLease(start)
	} else {
		err = start()
	}
	if err != nil {
		return out, errRefused
	}
	current, err := w.services.controller(ctx, nil)
	if err != nil || !sessionControllerRestarted(original.Controller, current) || paused() != nil || check() != nil {
		return out, errRefused
	}
	if a.absent(s, "controller.started.json") {
		if s.create("controller.started.json", current) != nil {
			return out, errRefused
		}
	}
	var started gatewayFacts
	if _, err := a.read(s, "controller.started.json", &started); err != nil || !equalSessionController(started, current) {
		return out, errRefused
	}
	listener, err := w.services.listener(ctx, current.Main)
	if err != nil || !listener.valid() {
		return out, errRefused
	}
	domain, err := w.domain(ctx, before, after, current)
	if err != nil || !digestPattern.MatchString(domain) || paused() != nil || check() != nil || w.installer.apply(ctx) != nil {
		return out, errRefused
	}
	again, err := w.services.controller(ctx, nil)
	if err != nil || !equalSessionController(current, again) {
		return out, errRefused
	}
	listenerAfter, err := w.services.listener(ctx, current.Main)
	if err != nil || listenerAfter != listener {
		return out, errRefused
	}
	out = sessionWindowReady{windowSHA, bytesSHA(s.raw["watchdog.drained.json"]), bytesSHA(s.raw["controller.stopped.json"]), config.AfterSHA, bytesSHA(s.raw["controller.started.json"]), domain, current, listener}
	if a.absent(s, "window.ready.json") {
		if s.create("window.ready.json", out) != nil {
			return sessionWindowReady{}, errRefused
		}
	}
	var ready sessionWindowReady
	if _, err := a.read(s, "window.ready.json", &ready); err != nil || !equalWindowReady(out, ready) || !s.unchanged() || s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil {
		return sessionWindowReady{}, errRefused
	}
	return out, nil
}

func equalSessionController(a, b gatewayFacts) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func equalWindowReady(a, b sessionWindowReady) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
