package lifecycle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sessionParent = "/var/lib/nucleus-halo-credentials"

type sessionPlan struct {
	Version    int    `json:"version"`
	Operation  string `json:"operation_id"`
	Controller string `json:"controller_id"`
	Username   string `json:"username"`
	Boot       string `json:"boot_id"`
	Invocation string `json:"controller_invocation"`
	BinarySHA  string `json:"controller_sha256"`
	ConfigSHA  string `json:"configuration_sha256"`
	TTL        int64  `json:"ttl_seconds"`
	Expires    int64  `json:"expires_at"`
}

func decodeSessionPlan(raw []byte) (sessionPlan, error) {
	var p sessionPlan
	if len(raw) > 64*1024 || strictObject(raw, &p) != nil || p.Version != 1 || !uuidPattern.MatchString(p.Operation) || !uuidPattern.MatchString(p.Controller) || p.Username != "haloadmin" || !uuidPattern.MatchString(p.Boot) || !pauseHex.MatchString(p.Invocation) || !digestPattern.MatchString(p.BinarySHA) || !digestPattern.MatchString(p.ConfigSHA) || p.TTL != 86400 || p.Expires <= 0 {
		return sessionPlan{}, errRefused
	}
	return p, nil
}

type sessionIntent struct {
	Version     int           `json:"version"`
	RequestSHA  string        `json:"request_sha256"`
	ProducerSHA string        `json:"producer_sha256"`
	Started     int64         `json:"started_at"`
	Origin      sessionOrigin `json:"origin"`
}

type sessionOrigin struct {
	WindowSHA  string `json:"window_sha256"`
	Invocation string `json:"controller_invocation"`
	ConfigSHA  string `json:"configuration_sha256"`
	User       string `json:"user_id"`
	Generation int64  `json:"generation"`
}

func (o sessionOrigin) valid(p sessionPlan) bool {
	return digestPattern.MatchString(o.WindowSHA) && pauseHex.MatchString(o.Invocation) && o.Invocation != p.Invocation && digestPattern.MatchString(o.ConfigSHA) && o.ConfigSHA != p.ConfigSHA && uuidPattern.MatchString(o.User) && o.Generation >= 0
}

func (o sessionOrigin) principal(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	var claims struct {
		User       *string `json:"user"`
		Generation *int64  `json:"generation"`
	}
	return err == nil && json.Unmarshal(raw, &claims) == nil && claims.User != nil && *claims.User == o.User && claims.Generation != nil && *claims.Generation == o.Generation
}

type sessionResponse struct {
	Status  int    `json:"status"`
	BodySHA string `json:"body_sha256"`
	Ended   int64  `json:"received_at"`
}

type sessionSeal struct {
	Version     int    `json:"version"`
	IntentSHA   string `json:"intent_sha256"`
	ResponseSHA string `json:"response_sha256"`
	TokenSHA    string `json:"token_sha256"`
	Expires     int64  `json:"expires_at"`
}

type sessionOutcome struct {
	State   string `json:"state"`
	Expires int64  `json:"expires_at,omitempty"`
}

// The production admission owns historical blockers, original locks and the
// loaded service/configuration binding. It remains held through publication.
// No journal field can turn an unverified admission into permission to send.
type sessionAdmission interface {
	check(context.Context) error
	origin() sessionOrigin
	password() (string, error)
	close()
}

type sessionAcquirer struct {
	files            protectedFiles
	parent, producer string
	admit            func(context.Context, sessionPlan, []byte, *serviceFiles, func(sessionAdmission) error) error
	fixture          http.RoundTripper
	now              func() time.Time
}

func (a sessionAcquirer) read(s *serviceFiles, name string, target any) ([]byte, error) {
	raw, err := s.files.readFile(filepath.Join(s.path, name), 0600, 64*1024, true)
	if err != nil || target != nil && strictObject(raw, target) != nil || s.track(name, raw) != nil {
		return nil, errRefused
	}
	return raw, nil
}

func (a sessionAcquirer) execute(ctx context.Context, request []byte) (sessionOutcome, error) {
	out := sessionOutcome{State: "unknown"}
	p, err := decodeSessionPlan(request)
	if err != nil || !digestPattern.MatchString(a.producer) || a.now == nil || a.admit == nil {
		return out, errRefused
	}
	s, err := openServiceFiles(a.files, a.parent, "session", "session.lock", true)
	if err != nil {
		return out, err
	}
	defer s.close()
	exists, err := s.existing()
	if err != nil {
		return out, err
	}
	var intent sessionIntent
	var originalIntent []byte
	if exists {
		raw, e := a.read(s, "request.json", nil)
		if e != nil || !bytes.Equal(raw, request) {
			return out, errRefused
		}
		originalIntent, err = a.read(s, "intent.json", &intent)
		if err != nil || intent.Version != 1 || intent.RequestSHA != bytesSHA(request) || intent.ProducerSHA != a.producer || intent.Started <= 0 {
			return out, errRefused
		}
		var seal sessionSeal
		if _, e := a.read(s, "complete.json", &seal); e == nil {
			if err := a.verifyOriginal(s, originalIntent, seal); err != nil {
				return out, err
			}
			return sessionOutcome{State: "original", Expires: seal.Expires}, nil
		} else if !a.absent(s, "complete.json") {
			return out, errRefused
		}
	}
	if ctx.Err() != nil || a.now().Unix() >= p.Expires {
		return out, errRefused
	}
	called := false
	err = a.admit(ctx, p, request, s, func(admission sessionAdmission) error {
		if called || admission == nil {
			return errRefused
		}
		called = true
		defer admission.close()
		var err error
		out, err = a.acquire(ctx, p, request, s, exists, intent, originalIntent, admission)
		return err
	})
	if err != nil || !called {
		return sessionOutcome{State: "unknown"}, errRefused
	}
	return out, nil
}

// The admission callback retains the original consumer/provider locks through
// login and publication. No goroutine outlives its owning maintenance window.
func (a sessionAcquirer) acquire(ctx context.Context, p sessionPlan, request []byte, s *serviceFiles, exists bool, intent sessionIntent, originalIntent []byte, admission sessionAdmission) (sessionOutcome, error) {
	out := sessionOutcome{State: "unknown"}
	origin := admission.origin()
	if !origin.valid(p) || exists && intent.Origin != origin || a.verifyWindow(s, p, origin, bytesSHA(request), a.producer) != nil {
		return out, errRefused
	}
	checkContext := func(c context.Context) error {
		if c.Err() != nil || a.now().Unix() >= p.Expires || !s.unchanged() || admission.check(c) != nil {
			return errRefused
		}
		return nil
	}
	check := func() error { return checkContext(ctx) }
	if check() != nil {
		return out, errRefused
	}
	if !exists {
		password, e := admission.password()
		if e != nil || password == "" || s.reserve() != nil {
			return out, errRefused
		}
		// request.json retains the exact trusted input; a different operation
		// cannot acquire another slot, including after an interrupted reserve.
		o := operation{files: s.files, parent: s.parent, dir: s.dir, path: s.path}
		if o.create("request.json", request) != nil || s.files.syncFile(s.dir) != nil || s.track("request.json", request) != nil {
			return out, errRefused
		}
		intent = sessionIntent{Version: 1, RequestSHA: bytesSHA(request), ProducerSHA: a.producer, Started: a.now().Unix(), Origin: origin}
		if s.create("intent.json", intent) != nil || check() != nil {
			return out, errRefused
		}
		originalIntent = s.raw["intent.json"]
		client := newBoundSessionClient(func(status int, body []byte) error {
			if !s.unchanged() || o.create("response.body", body) != nil || s.files.syncFile(s.dir) != nil || s.track("response.body", body) != nil {
				return errRefused
			}
			return s.create("response.json", sessionResponse{status, bytesSHA(body), a.now().Unix()})
		}, a.fixture, checkContext)
		_, e = client.login(ctx, p.Username, password)
		client.close()
		if e != nil {
			return out, errRefused
		}
	}
	// Once a slot exists, no path below calls Login or rereads the password.
	var response sessionResponse
	responseRaw, err := a.read(s, "response.json", &response)
	if err != nil || response.Status != 200 || response.Ended < intent.Started {
		return out, errRefused
	}
	body, err := a.read(s, "response.body", nil)
	if err != nil || bytesSHA(body) != response.BodySHA {
		return out, errRefused
	}
	var wire struct {
		Token string `json:"token"`
	}
	if strictObject(body, &wire) != nil {
		return out, errRefused
	}
	expiry, err := sessionExpiry(wire.Token, time.Unix(intent.Started, 0), time.Unix(response.Ended, 0))
	if err != nil || !origin.principal(wire.Token) || a.now().Unix() >= expiry || check() != nil {
		return out, errRefused
	}
	client := newBoundSessionClient(nil, a.fixture, checkContext)
	defer client.close()
	if client.controller(ctx, wire.Token, p.Controller) != nil || check() != nil {
		return out, errRefused
	}
	tokenRaw := []byte(wire.Token + "\n")
	seal := sessionSeal{1, bytesSHA(originalIntent), bytesSHA(responseRaw), bytesSHA(tokenRaw), expiry}
	// Seal the verified origin before exposing the consumer's raw token file.
	// A missing final delivery seal can then be recovered without inventing the
	// original acceptance judgment or sending another Login.
	if a.absent(s, "verified.json") {
		if s.create("verified.json", seal) != nil {
			return out, errRefused
		}
	}
	var verified sessionSeal
	if _, e := a.read(s, "verified.json", &verified); e != nil || verified != seal || check() != nil {
		return out, errRefused
	}
	if a.absent(s, "garm-token") {
		o := operation{files: s.files, parent: s.parent, dir: s.dir, path: s.path}
		if o.create("garm-token", tokenRaw) != nil || s.files.syncFile(s.dir) != nil {
			return out, errRefused
		}
	}
	if raw, e := a.read(s, "garm-token", nil); e != nil || !bytes.Equal(raw, tokenRaw) || check() != nil {
		return out, errRefused
	}
	if s.create("complete.json", seal) != nil {
		return out, errRefused
	}
	return sessionOutcome{State: "acquired", Expires: expiry}, nil
}

func (a sessionAcquirer) absent(s *serviceFiles, name string) bool {
	_, err := s.files.read(filepath.Join(s.path, name), 0600, 64*1024)
	return os.IsNotExist(err) && s.unchanged()
}

func (a sessionAcquirer) verifyOriginal(s *serviceFiles, intent []byte, seal sessionSeal) error {
	// A prior directory sync may have failed after the seal was created.
	// Establish durability of the original publication without rewriting it.
	if s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil {
		return errRefused
	}
	var verified sessionSeal
	if _, err := a.read(s, "verified.json", &verified); err != nil || verified != seal {
		return errRefused
	}
	var response sessionResponse
	raw, err := a.read(s, "response.json", &response)
	if err != nil || seal.Version != 1 || seal.IntentSHA != bytesSHA(intent) || seal.ResponseSHA != bytesSHA(raw) || response.Status != 200 {
		return errRefused
	}
	body, err := a.read(s, "response.body", nil)
	if err != nil || bytesSHA(body) != response.BodySHA {
		return errRefused
	}
	var wire struct {
		Token string `json:"token"`
	}
	var i sessionIntent
	if strictObject(body, &wire) != nil || strictObject(intent, &i) != nil {
		return errRefused
	}
	p, err := decodeSessionPlan(s.raw["request.json"])
	if err != nil || i.Version != 1 || i.RequestSHA != bytesSHA(s.raw["request.json"]) || !digestPattern.MatchString(i.ProducerSHA) || !i.Origin.valid(p) || !i.Origin.principal(wire.Token) || a.verifyWindow(s, p, i.Origin, i.RequestSHA, i.ProducerSHA) != nil {
		return errRefused
	}
	expiry, err := sessionExpiry(wire.Token, time.Unix(i.Started, 0), time.Unix(response.Ended, 0))
	if err != nil || seal.Expires != expiry {
		return errRefused
	}
	expected := []byte(wire.Token + "\n")
	if seal.TokenSHA != bytesSHA(expected) {
		return errRefused
	}
	// The derived consumer copy may be removed only after a retained expiry
	// rejection. Original response and acceptance records remain protected.
	if a.absent(s, "garm-token") {
		_, err := a.retirementRejection(s, seal)
		return err
	}
	token, err := a.read(s, "garm-token", nil)
	if err != nil || !bytes.Equal(token, expected) || !s.unchanged() {
		return errRefused
	}
	return nil
}

func (a sessionAcquirer) verifyWindow(s *serviceFiles, p sessionPlan, origin sessionOrigin, requestSHA, producer string) error {
	window, err := s.sibling("window")
	if err != nil {
		return errRefused
	}
	defer window.close()
	if exists, err := window.existing(); err != nil || !exists {
		return errRefused
	}
	var ready sessionWindowReady
	raw, err := a.read(window, "window.ready.json", &ready)
	if err != nil || bytesSHA(raw) != origin.WindowSHA || ready.After.Invocation != origin.Invocation || ready.ConfigSHA != origin.ConfigSHA || !sessionControllerRunning(ready.After) || !ready.Listener.valid() || ready.After.Main.ExecutableSHA != p.BinarySHA {
		return errRefused
	}
	for _, sha := range []string{ready.IntentSHA, ready.PausedSHA, ready.StopSHA, ready.StartSHA, ready.DomainSHA} {
		if !digestPattern.MatchString(sha) {
			return errRefused
		}
	}
	var intent sessionWindowIntent
	raw, err = a.read(window, "window.intent.json", &intent)
	if err != nil || bytesSHA(raw) != ready.IntentSHA || intent.Version != 1 || intent.RequestSHA != requestSHA || intent.ProducerSHA != producer || intent.Boot != p.Boot || intent.Controller.Invocation != p.Invocation || !sessionControllerRunning(intent.Controller) || !intent.Listener.valid() || !sessionControllerRestarted(intent.Controller, ready.After) || !window.unchanged() {
		return errRefused
	}
	var paused pauseUnits
	raw, err = a.read(window, "watchdog.drained.json", &paused)
	if err != nil || bytesSHA(raw) != ready.PausedSHA || !pauseDrainedShape(paused) || pauseCompatible(intent.Watchdog, paused) != nil {
		return errRefused
	}
	var stopped, started gatewayFacts
	raw, err = a.read(window, "controller.stopped.json", &stopped)
	if err != nil || bytesSHA(raw) != ready.StopSHA || !sessionControllerStopped(intent.Controller, stopped) {
		return errRefused
	}
	raw, err = a.read(window, "controller.started.json", &started)
	if err != nil || bytesSHA(raw) != ready.StartSHA || !equalSessionController(started, ready.After) {
		return errRefused
	}
	var domain sessionDomainProof
	raw, err = a.read(window, "old-domain.rejected.json", &domain)
	if err != nil || bytesSHA(raw) != ready.DomainSHA || domain.Old != p.Invocation || domain.New != origin.Invocation || domain.Rejected != 401 || domain.Accepted != 200 || !digestPattern.MatchString(domain.ProbeSHA) {
		return errRefused
	}
	for name, expected := range map[string]string{"configuration.before.toml": p.ConfigSHA, "configuration.after.toml": origin.ConfigSHA} {
		raw, err := a.read(window, name, nil)
		if err != nil || bytesSHA(raw) != expected {
			return errRefused
		}
	}
	var installed sessionConfigStage
	raw, err = a.read(window, "configuration.installed.json", &installed)
	if err != nil || !digestPattern.MatchString(installed.IntentSHA) || installed.Device == 0 || installed.Inode == 0 {
		return errRefused
	}
	installedSHA := bytesSHA(raw)
	if a.verifyWindowJob(window, "watchdog", sessionWindowJob{ready.IntentSHA, ready.IntentSHA, pauseTimer, "StopUnit"}, managerOwner(intent.Manager)) != nil || a.verifyWindowJob(window, "controller.stop", sessionWindowJob{ready.IntentSHA, ready.PausedSHA, controllerUnit, "StopUnit"}, managerOwner(intent.Manager)) != nil || a.verifyWindowJob(window, "controller.start", sessionWindowJob{ready.IntentSHA, installedSHA, controllerUnit, "StartUnit"}, managerOwner(intent.Manager)) != nil {
		return errRefused
	}
	return nil
}

func (a sessionAcquirer) verifyWindowJob(s *serviceFiles, name string, expected sessionWindowJob, sender string) error {
	if !pauseOwner.MatchString(sender) {
		return errRefused
	}
	var intent sessionWindowJob
	raw, err := a.read(s, name+".intent.json", &intent)
	if err != nil || intent != expected {
		return errRefused
	}
	sha := bytesSHA(raw)
	var job pauseJob
	if _, err := a.read(s, name+".job.json", &job); err != nil || job.IntentSHA != sha || job.ID == 0 || job.Path != pauseJobPath(job.ID) {
		return errRefused
	}
	var event pauseEvent
	if _, err := a.read(s, name+".event.json", &event); err != nil || event.IntentSHA != sha || event.ID != job.ID || event.Path != job.Path || event.Unit != expected.Unit || event.Sender != sender || event.Result != "done" || !s.unchanged() {
		return errRefused
	}
	return nil
}
