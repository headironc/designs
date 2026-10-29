package lifecycle

import (
	"context"
	"strings"
	"time"
)

type connectionIntent struct {
	Version    int            `json:"version"`
	Request    string         `json:"request_sha256"`
	Producer   string         `json:"producer_sha256"`
	Started    int64          `json:"started_at"`
	Origin     observerOrigin `json:"origin"`
	Boot       string         `json:"boot_id"`
	Manager    string         `json:"manager"`
	Controller gatewayFacts   `json:"controller"`
}

type connectionControl interface {
	identity(context.Context) (string, string, error)
	controller(context.Context, *gatewayFacts) (gatewayFacts, error)
	stopController(context.Context) (pauseJob, error)
	event(context.Context, pauseJob, string) (pauseEvent, error)
	controllerDrained(context.Context, gatewayFacts, gatewayFacts) error
}

type connectionStopIntent struct {
	Connection string `json:"connection_intent_sha256"`
	Write      string `json:"write_intent_sha256"`
	Step       string `json:"step"`
}

type connectionStopped struct {
	Intent     string       `json:"stop_intent_sha256"`
	Job        string       `json:"job_sha256"`
	Event      string       `json:"event_sha256"`
	Observed   int64        `json:"observed_at"`
	Controller gatewayFacts `json:"controller"`
}

// This closes the original local controller only. The external create and any
// compensation remain unknown without their own original success receipts.
func stopConnectionController(ctx context.Context, s *serviceFiles, p connectionPlan, original connectionIntent, step string, control connectionControl, now func() time.Time) error {
	if control == nil || now == nil || step != "repository-create" && step != "scaleset-create" || !sessionControllerRunning(original.Controller) || original.Request != bytesSHA(s.raw["request.json"]) || original.Boot == "" || managerOwner(original.Manager) == "" {
		return errRefused
	}
	reader := sessionAcquirer{files: s.files}
	var write observerCall
	if _, err := reader.read(s, step+".intent.json", &write); err != nil || write.Request != original.Request || write.Producer != original.Producer || write.Method != "POST" || write.Scheme != "http" || write.Host != "127.0.0.1:9997" || write.AuthScheme != "Bearer" || write.Step != step {
		return errRefused
	}
	if step == "repository-create" && write.Path != "/api/v1/repositories" || step == "scaleset-create" && (!strings.HasPrefix(write.Path, "/api/v1/repositories/") || !strings.HasSuffix(write.Path, "/scalesets") || !uuidPattern.MatchString(strings.TrimSuffix(strings.TrimPrefix(write.Path, "/api/v1/repositories/"), "/scalesets"))) {
		return errRefused
	}
	intent := connectionStopIntent{bytesSHA(s.raw["connection.intent.json"]), bytesSHA(s.raw[step+".intent.json"]), step}
	identity := func() error {
		boot, manager, err := control.identity(ctx)
		if err != nil || boot != original.Boot || manager != original.Manager || ctx.Err() != nil || connectionClock(s, now().Unix()) != nil {
			return errRefused
		}
		return nil
	}
	// An original event already sealed is a fact; only current drain checks
	// remain. No replacement StopUnit or event query is needed after sealing.
	if !reader.absent(s, "controller.stopped.json") {
		var savedIntent connectionStopIntent
		var savedJob pauseJob
		var savedEvent pauseEvent
		if _, err := reader.read(s, "controller.stop.intent.json", &savedIntent); err != nil || savedIntent != intent {
			return errRefused
		}
		if _, err := reader.read(s, "controller.stop.job.json", &savedJob); err != nil || savedJob.IntentSHA != bytesSHA(s.raw["controller.stop.intent.json"]) || savedJob.ID == 0 || savedJob.Path != pauseJobPath(savedJob.ID) {
			return errRefused
		}
		if _, err := reader.read(s, "controller.stop.event.json", &savedEvent); err != nil || savedEvent.IntentSHA != savedJob.IntentSHA || savedEvent.ID != savedJob.ID || savedEvent.Path != savedJob.Path || savedEvent.Unit != controllerUnit || savedEvent.Sender != managerOwner(original.Manager) || savedEvent.Result != "done" {
			return errRefused
		}
		var stopped connectionStopped
		if _, err := reader.read(s, "controller.stopped.json", &stopped); err != nil || stopped.Intent != bytesSHA(s.raw["controller.stop.intent.json"]) || stopped.Job != bytesSHA(s.raw["controller.stop.job.json"]) || stopped.Event != bytesSHA(s.raw["controller.stop.event.json"]) || !sessionControllerStopped(original.Controller, stopped.Controller) {
			return errRefused
		}
		return identity()
	}
	if identity() != nil {
		return errRefused
	}
	newIntent := reader.absent(s, "controller.stop.intent.json")
	if newIntent {
		current, err := control.controller(ctx, &original.Controller)
		if err != nil || !equalSessionController(current, original.Controller) || s.create("controller.stop.intent.json", intent) != nil || identity() != nil {
			return errRefused
		}
	} else {
		var saved connectionStopIntent
		if _, err := reader.read(s, "controller.stop.intent.json", &saved); err != nil || saved != intent {
			return errRefused
		}
	}
	intentSHA := bytesSHA(s.raw["controller.stop.intent.json"])
	if newIntent {
		job, err := control.stopController(ctx)
		if err != nil || job.ID == 0 || job.Path != pauseJobPath(job.ID) {
			return errRefused
		}
		job.IntentSHA = intentSHA
		if s.create("controller.stop.job.json", job) != nil {
			return errRefused
		}
	}
	var job pauseJob
	if _, err := reader.read(s, "controller.stop.job.json", &job); err != nil || job.IntentSHA != intentSHA || job.ID == 0 || job.Path != pauseJobPath(job.ID) {
		return errRefused
	}
	validEvent := func(event pauseEvent) bool {
		return event.IntentSHA == intentSHA && event.ID == job.ID && event.Path == job.Path && event.Unit == controllerUnit && event.Sender == managerOwner(original.Manager) && pauseJobResult(event.Result)
	}
	if reader.absent(s, "controller.stop.event.json") {
		event, err := control.event(ctx, job, controllerUnit)
		if err != nil {
			return errRefused
		}
		event.IntentSHA = intentSHA
		if !validEvent(event) || s.create("controller.stop.event.json", event) != nil {
			return errRefused
		}
	}
	var event pauseEvent
	if _, err := reader.read(s, "controller.stop.event.json", &event); err != nil || !validEvent(event) || event.Result != "done" || identity() != nil {
		return errRefused
	}
	current, err := control.controller(ctx, &original.Controller)
	if err != nil || !sessionControllerStopped(original.Controller, current) || control.controllerDrained(ctx, original.Controller, current) != nil {
		return errRefused
	}
	again, err := control.controller(ctx, &original.Controller)
	if err != nil || !equalSessionController(current, again) || identity() != nil {
		return errRefused
	}
	return s.create("controller.stopped.json", connectionStopped{intentSHA, bytesSHA(s.raw["controller.stop.job.json"]), bytesSHA(s.raw["controller.stop.event.json"]), now().Unix(), current})
}
