package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cloudbase/garm/params"
)

type connectionAdmission struct {
	app           appAdmission
	control       connectionControl
	boot, manager string
	controller    gatewayFacts
	check         func(context.Context) error
	webhook       func() (string, error)
}

type connectionExecutor struct {
	files            protectedFiles
	parent, producer string
	now              func() time.Time
	fixture          http.RoundTripper
	admit            func(context.Context, connectionPlan, *serviceFiles, func(connectionAdmission) error) error
	recoverStop      func(context.Context, *serviceFiles, connectionPlan, connectionIntent, string) error
}

type connectionVerification struct {
	Binding        connectionBinding `json:"binding"`
	RepositoryRead string            `json:"repository_read"`
	ScaleRead      string            `json:"scaleset_read"`
	ActionsRead    string            `json:"actions_read"`
	FinalScaleRead string            `json:"final_scaleset_read"`
	Records        map[string]string `json:"records"`
	Observed       int64             `json:"observed_at"`
}

func (e connectionExecutor) execute(ctx context.Context, raw []byte) (connectionBinding, string, error) {
	p, err := decodeConnectionPlan(raw)
	if err != nil || !digestPattern.MatchString(e.producer) || e.now == nil || e.admit == nil {
		return connectionBinding{}, "arguments", errRefused
	}
	s, err := openServiceFiles(e.files, e.parent, "connection", "session.lock", false)
	if err != nil {
		return connectionBinding{}, "lease", errRefused
	}
	defer s.close()
	reader := sessionAcquirer{files: e.files}
	exists, err := s.existing()
	if err != nil {
		return connectionBinding{}, "store", errRefused
	}
	var original connectionIntent
	if exists {
		saved, err := reader.read(s, "request.json", nil)
		if err != nil || !bytes.Equal(saved, raw) {
			return connectionBinding{}, "request", errRefused
		}
		if _, err := reader.read(s, "connection.intent.json", &original); err != nil || original.Version != 1 || original.Request != bytesSHA(raw) || original.Producer != e.producer || original.Started <= 0 || e.now().Unix() < original.Started {
			return connectionBinding{}, "origin", errRefused
		}
		if !reader.absent(s, "controller.stop.intent.json") {
			var stop connectionStopIntent
			if _, err := reader.read(s, "controller.stop.intent.json", &stop); err != nil || e.recoverStop == nil || e.recoverStop(ctx, s, p, original, stop.Step) != nil {
				return connectionBinding{}, "original_stop_unknown", errRefused
			}
			return connectionBinding{}, "original_create_unknown_controller_stopped", errRefused
		}
		if !reader.absent(s, "verified.json") {
			binding, err := e.publish(s, p)
			return binding, "original", err
		}
		if !reader.absent(s, "retirement.intent.json") {
			return connectionBinding{}, "retirement_started", errRefused
		}
		// A previous send without an acceptable native receipt may only finish
		// its original controller stop; it cannot obtain new API credentials.
		for _, step := range []string{"repository-create", "scaleset-create"} {
			if !reader.absent(s, step+".intent.json") && !connectionCreateKnown(s, p, e.producer, step) {
				if e.recoverStop == nil || e.recoverStop(ctx, s, p, original, step) != nil {
					return connectionBinding{}, "original_create_and_stop_unknown", errRefused
				}
				return connectionBinding{}, "original_create_unknown_controller_stopped", errRefused
			}
		}
	}
	var binding connectionBinding
	stage := "admission"
	called := false
	err = e.admit(ctx, p, s, func(ad connectionAdmission) error {
		if called || ad.check == nil || ad.webhook == nil || ad.app.key == nil || ad.control == nil || !sessionControllerRunning(ad.controller) || ad.boot == "" || managerOwner(ad.manager) == "" {
			return errRefused
		}
		called = true
		if ad.check(ctx) != nil || e.now().Unix() >= p.Source.RequiredUntil {
			return errRefused
		}
		if !exists {
			if s.reserve() != nil || observerWriteRaw(s, "request.json", raw) != nil {
				return errRefused
			}
			original = connectionIntent{1, bytesSHA(raw), e.producer, e.now().Unix(), ad.app.origin, ad.boot, ad.manager, ad.controller}
			if s.create("connection.intent.json", original) != nil {
				return errRefused
			}
		} else if original.Boot != ad.boot || original.Manager != ad.manager || !equalSessionController(original.Controller, ad.controller) || original.Origin.Session != ad.app.origin.Session || original.Origin.Invocation != ad.app.origin.Invocation || original.Origin.Config != ad.app.origin.Config {
			return errRefused
		}
		check := func(c context.Context) error {
			if c.Err() != nil || e.now().Unix() >= p.Source.RequiredUntil || e.now().Unix() < original.Started || connectionClock(s, e.now().Unix()) != nil || ad.check(c) != nil {
				return errRefused
			}
			return nil
		}
		api := connectionAPI{store: s, plan: p, producer: e.producer, garmToken: ad.app.token, now: e.now, check: check, fixture: e.fixture}
		defer api.close()
		stage = "credentials"
		actions, err := api.acquireCredentials(ctx, ad.app)
		if err != nil || e.now().Unix() >= actions.Expires {
			return errRefused
		}
		liveCheck := func() error {
			if check(ctx) != nil || api.credential(ctx) != nil || api.template(ctx) != nil {
				return errRefused
			}
			return nil
		}
		if liveCheck() != nil {
			return errRefused
		}
		stage = "repository"
		var repo params.Repository
		if p.RepositoryMode == "existing" {
			repo, _, err = api.repository(ctx, p.Repository)
			if err != nil || connectionRepositoryIdentity(repo) != p.RepositorySHA {
				return errRefused
			}
		} else {
			if reader.absent(s, "repository-create.intent.json") {
				if api.inventory(ctx, "", 0) != nil || connectionRemoteEmpty(ctx, &api, actions) != nil || liveCheck() != nil {
					return errRefused
				}
				secret, err := ad.webhook()
				if err != nil {
					return errRefused
				}
				repo, err = api.createRepository(ctx, secret)
				if err != nil {
					return e.failedCreate(s, p, original, "repository-create", ad.control)
				}
			} else {
				r, err := api.response("repository-create")
				if err != nil {
					return errRefused
				}
				repo, err = connectionRepository(r.Body, p, false)
				if err != nil {
					return errRefused
				}
			}
		}
		current, _, err := api.repository(ctx, repo.ID)
		if err != nil || connectionRepositoryIdentity(current) != connectionRepositoryIdentity(repo) {
			return errRefused
		}
		stage = "scaleset"
		var scale params.ScaleSet
		if reader.absent(s, "scaleset-create.intent.json") {
			if api.inventory(ctx, repo.ID, 0) != nil || connectionRemoteEmpty(ctx, &api, actions) != nil || liveCheck() != nil {
				return errRefused
			}
			scale, err = api.createScale(ctx, repo.ID)
			if err != nil {
				return e.failedCreate(s, p, original, "scaleset-create", ad.control)
			}
		} else {
			r, err := api.response("scaleset-create")
			if err != nil {
				return errRefused
			}
			scale, err = connectionScale(r.Body, p, repo.ID, false)
			if err != nil {
				return errRefused
			}
		}
		stage = "verification"
		if api.inventory(ctx, repo.ID, scale.ID) != nil || liveCheck() != nil || e.now().Unix() >= actions.Expires {
			return errRefused
		}
		repoNow, repoStep, err := api.repository(ctx, repo.ID)
		if err != nil || connectionRepositoryIdentity(repoNow) != connectionRepositoryIdentity(repo) {
			return errRefused
		}
		scaleNow, scaleStep, err := api.scale(ctx, scale.ID, repo.ID)
		if err != nil || scaleNow.ScaleSetID != scale.ScaleSetID {
			return errRefused
		}
		actionsStep, err := api.nextRead("actions")
		if err != nil {
			return errRefused
		}
		remote, err := api.actionsRead(ctx, actions, actionsStep)
		if err != nil || remote.Status != 200 || remote.Link != "" || connectionRemote(remote.Body, p, scaleNow) != nil || check(ctx) != nil {
			return errRefused
		}
		// Repeat the current local inventory after the remote read; an original
		// create receipt cannot authorize exporting a later replacement ID.
		if api.inventory(ctx, repo.ID, scale.ID) != nil {
			return errRefused
		}
		last, finalScaleStep, err := api.scale(ctx, scale.ID, repo.ID)
		if err != nil || last.ScaleSetID != scale.ScaleSetID || check(ctx) != nil {
			return errRefused
		}
		binding = connectionBinding{Version: 1, Request: bytesSHA(raw), Producer: e.producer, Controller: p.Source.Controller, Installation: p.Source.Installation, Repository: repo.ID, ScaleSet: scale.ID, GitHubScaleSet: scale.ScaleSetID, ScaleResponse: bytesSHA(s.raw["scaleset-create.response.json"]), Enabled: false, CredentialState: "installation_tokens_retired_other_expiry_pending"}
		if p.RepositoryMode == "create" {
			binding.RepositoryResponse = bytesSHA(s.raw["repository-create.response.json"])
		} else {
			binding.RepositoryResponse = p.RepositorySHA
		}
		binding.GARMObservation = bytesSHA(append(append([]byte(nil), s.raw[repoStep+".response.json"]...), s.raw[scaleStep+".response.json"]...))
		binding.GitHubObservation = bytesSHA(s.raw[actionsStep+".response.json"])
		records := map[string]string{}
		for name, data := range s.raw {
			if name != "github-token" && name != "garm-token" {
				records[name] = bytesSHA(data)
			}
		}
		if s.create("verified.json", connectionVerification{Binding: binding, RepositoryRead: repoStep, ScaleRead: scaleStep, ActionsRead: actionsStep, FinalScaleRead: finalScaleStep, Records: records, Observed: e.now().Unix()}) != nil {
			return errRefused
		}
		stage = "publication"
		binding, err = e.publish(s, p)
		return err
	})
	if err != nil || !called {
		return connectionBinding{}, stage, errRefused
	}
	return binding, "disabled_connection_verified_credentials_pending", nil
}

func connectionCreateKnown(s *serviceFiles, p connectionPlan, producer, step string) bool {
	api := connectionAPI{store: s, plan: p, producer: producer}
	r, err := api.response(step)
	if err != nil || r.Status != 200 {
		return false
	}
	if step == "repository-create" {
		_, err = connectionRepository(r.Body, p, false)
		return err == nil
	}
	repository := p.Repository
	if p.RepositoryMode == "create" {
		created, err := api.response("repository-create")
		if err != nil || created.Status != 200 {
			return false
		}
		repo, err := connectionRepository(created.Body, p, false)
		if err != nil {
			return false
		}
		repository = repo.ID
	}
	_, err = connectionScale(r.Body, p, repository, false)
	return err == nil
}

func connectionRemoteEmpty(ctx context.Context, api *connectionAPI, actions connectionActions) error {
	step, err := api.nextRead("actions-empty")
	if err != nil {
		return err
	}
	r, err := api.actionsRead(ctx, actions, step)
	var body params.RunnerScaleSetsResponse
	if err != nil || r.Status != 200 || r.Link != "" || !connectionWire(r.Body, "count", "value") || json.Unmarshal(r.Body, &body) != nil || body.Count != 0 || body.RunnerScaleSets == nil || len(body.RunnerScaleSets) != 0 {
		return errRefused
	}
	return nil
}

func (e connectionExecutor) failedCreate(s *serviceFiles, p connectionPlan, original connectionIntent, step string, control connectionControl) error {
	if (sessionAcquirer{files: s.files}).absent(s, step+".intent.json") {
		return errRefused
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_ = stopConnectionController(ctx, s, p, original, step, control, e.now)
	return errRefused
}

func validateConnection(s *serviceFiles, p connectionPlan, producer string, now int64) (connectionBinding, error) {
	reader := sessionAcquirer{files: s.files}
	var verified connectionVerification
	if _, err := reader.read(s, "verified.json", &verified); err != nil || verified.Observed <= 0 || verified.Observed > now || verified.Binding.Version != 1 || verified.Binding.Request != bytesSHA(s.raw["request.json"]) || verified.Binding.Producer != producer || verified.Binding.Enabled || verified.Binding.Controller != p.Source.Controller || verified.Binding.Installation != p.Source.Installation || !uuidPattern.MatchString(verified.Binding.Repository) || verified.Binding.ScaleSet == 0 || verified.Binding.GitHubScaleSet <= 0 || len(verified.Records) == 0 || len(verified.Records) > 512 {
		return connectionBinding{}, errRefused
	}
	for name, sum := range verified.Records {
		if filepath.Base(name) != name || name == "verified.json" || name == "connection.json" || name == "complete.json" || !digestPattern.MatchString(sum) {
			return connectionBinding{}, errRefused
		}
		raw, err := reader.read(s, name, nil)
		if err != nil || bytesSHA(raw) != sum {
			return connectionBinding{}, errRefused
		}
	}
	required := []string{"request.json", "connection.intent.json", "sign.intent.json", "sign.response.json"}
	steps := []string{"installation-before", "installation-after", "audit-mint", "audit-repositories", "audit-delete", "audit-rejection", "admin-mint", "admin-repositories", "admin-delete", "admin-rejection", "registration", "remote-auth", "scaleset-create", verified.RepositoryRead, verified.ScaleRead, verified.ActionsRead, verified.FinalScaleRead}
	if p.RepositoryMode == "create" {
		steps = append(steps, "repository-create")
	}
	for _, step := range steps {
		if step == "" || filepath.Base(step) != step {
			return connectionBinding{}, errRefused
		}
		required = append(required, step+".intent.json", step+".response.json")
	}
	for _, name := range required {
		if len(s.raw[name]) == 0 || verified.Records[name] != bytesSHA(s.raw[name]) {
			return connectionBinding{}, errRefused
		}
	}
	if connectionClock(s, now) != nil {
		return connectionBinding{}, errRefused
	}
	var origin connectionIntent
	if strictObject(s.raw["connection.intent.json"], &origin) != nil || origin.Version != 1 || origin.Request != verified.Binding.Request || origin.Producer != producer || !sessionControllerRunning(origin.Controller) || origin.Started <= 0 || origin.Started > verified.Observed {
		return connectionBinding{}, errRefused
	}
	api := connectionAPI{store: s, plan: p, producer: producer}
	repo, err := api.response(verified.RepositoryRead)
	if err != nil || repo.Status != 200 {
		return connectionBinding{}, errRefused
	}
	r, err := connectionRepository(repo.Body, p, true)
	if err != nil || r.ID != verified.Binding.Repository {
		return connectionBinding{}, errRefused
	}
	scale, err := api.response(verified.ScaleRead)
	if err != nil || scale.Status != 200 {
		return connectionBinding{}, errRefused
	}
	sc, err := connectionScale(scale.Body, p, r.ID, true)
	if err != nil || sc.ID != verified.Binding.ScaleSet || sc.ScaleSetID != verified.Binding.GitHubScaleSet {
		return connectionBinding{}, errRefused
	}
	remote, err := api.response(verified.ActionsRead)
	if err != nil || remote.Status != 200 || remote.Link != "" || connectionRemote(remote.Body, p, sc) != nil {
		return connectionBinding{}, errRefused
	}
	if p.RepositoryMode == "create" && !connectionCreateKnown(s, p, producer, "repository-create") || !connectionCreateKnown(s, p, producer, "scaleset-create") {
		return connectionBinding{}, errRefused
	}
	if p.RepositoryMode == "existing" && (r.ID != p.Repository || connectionRepositoryIdentity(r) != p.RepositorySHA || verified.Binding.RepositoryResponse != p.RepositorySHA) {
		return connectionBinding{}, errRefused
	}
	if p.RepositoryMode == "create" {
		created, err := api.response("repository-create")
		if err != nil {
			return connectionBinding{}, errRefused
		}
		original, err := connectionRepository(created.Body, p, false)
		if err != nil || original.ID != r.ID || connectionRepositoryIdentity(original) != connectionRepositoryIdentity(r) || verified.Binding.RepositoryResponse != bytesSHA(s.raw["repository-create.response.json"]) {
			return connectionBinding{}, errRefused
		}
	}
	created, err := api.response("scaleset-create")
	if err != nil {
		return connectionBinding{}, errRefused
	}
	originalScale, err := connectionScale(created.Body, p, r.ID, false)
	if err != nil || originalScale.ID != sc.ID || originalScale.ScaleSetID != sc.ScaleSetID || verified.Binding.ScaleResponse != bytesSHA(s.raw["scaleset-create.response.json"]) {
		return connectionBinding{}, errRefused
	}
	last, err := api.response(verified.FinalScaleRead)
	if err != nil || last.Status != 200 {
		return connectionBinding{}, errRefused
	}
	finalScale, err := connectionScale(last.Body, p, r.ID, true)
	if err != nil || finalScale.ID != sc.ID || finalScale.ScaleSetID != sc.ScaleSetID {
		return connectionBinding{}, errRefused
	}
	observations := append(append([]byte(nil), s.raw[verified.RepositoryRead+".response.json"]...), s.raw[verified.ScaleRead+".response.json"]...)
	if verified.Binding.GARMObservation != bytesSHA(observations) || verified.Binding.GitHubObservation != bytesSHA(s.raw[verified.ActionsRead+".response.json"]) || verified.Binding.CredentialState != "installation_tokens_retired_other_expiry_pending" {
		return connectionBinding{}, errRefused
	}
	return verified.Binding, nil
}

func (e connectionExecutor) publish(s *serviceFiles, p connectionPlan) (connectionBinding, error) {
	expected, err := validateConnection(s, p, e.producer, e.now().Unix())
	if err != nil {
		return connectionBinding{}, errRefused
	}
	reader := sessionAcquirer{files: s.files}
	if reader.absent(s, "connection.json") {
		if s.create("connection.json", expected) != nil {
			return connectionBinding{}, errRefused
		}
	}
	var saved connectionBinding
	if _, err := reader.read(s, "connection.json", &saved); err != nil || saved != expected {
		return connectionBinding{}, errRefused
	}
	seal := bytesSHA(s.raw["verified.json"])
	if reader.absent(s, "complete.json") {
		if s.create("complete.json", struct {
			Verified string `json:"verified_sha256"`
		}{seal}) != nil {
			return connectionBinding{}, errRefused
		}
	}
	var complete struct {
		Verified string `json:"verified_sha256"`
	}
	if _, err := reader.read(s, "complete.json", &complete); err != nil || complete.Verified != seal || s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil || !s.unchanged() {
		return connectionBinding{}, errRefused
	}
	return saved, nil
}
