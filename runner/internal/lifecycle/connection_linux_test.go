package lifecycle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudbase/garm/params"
	"github.com/headironc/designs/runner/internal/provider"
)

type connectionFixture struct {
	observer                  *observerFixture
	executor                  connectionExecutor
	plan                      connectionPlan
	raw                       []byte
	bus                       *sessionWindowFixtureBus
	repo                      params.Repository
	scale                     params.ScaleSet
	createdRepo, createdScale bool
	posts                     map[string]int
	requests                  int
	mutate                    func(*http.Request, *http.Response) error
}

func connectionFixtureFor(t *testing.T) *connectionFixture {
	t.Helper()
	o := observerFixtureFor(t)
	_, _, _, bus := sessionWindowFixture(t)
	id := uint(7)
	bootstrap := []byte("#!/bin/sh\nexec /usr/local/bin/runner-bootstrap\n")
	extra, _ := json.Marshal(map[string]any{"disable_updates": true, "runner_install_template": bootstrap})
	f := &connectionFixture{observer: o, bus: bus, posts: map[string]int{}}
	o.mutate = func(r *http.Request, response *http.Response) error {
		if f.mutate != nil {
			return f.mutate(r, response)
		}
		return nil
	}
	f.plan = connectionPlan{Version: 1, Source: o.plan, RepositoryMode: "create", WebhookSHA: bytesSHA([]byte("synthetic webhook input with enough entropy")), TemplateSHA: bytesSHA([]byte("synthetic system template")), BootstrapSHA: bytesSHA(bootstrap), ImageSHA: strings.Repeat("4", 64), Scale: params.CreateScaleSetParams{RunnerPrefix: params.RunnerPrefix{Prefix: "halo"}, Name: "nucleus-isolated", DisableUpdate: true, ProviderName: "halo-lxd", MaxRunners: 1, Image: "halo-image", Flavor: "halo-ci", OSType: "linux", OSArch: "amd64", RunnerBootstrapTimeout: 300, ExtraSpecs: extra, GitHubRunnerGroup: "Default", TemplateID: &id}}
	f.repo = params.Repository{ID: "cccccccc-0000-4000-8000-000000000003", Owner: "headironc", Name: "nucleus", CredentialsID: 1, Endpoint: params.ForgeEndpoint{Name: "public", BaseURL: "https://github.com", APIBaseURL: "https://api.github.com", UploadBaseURL: "https://uploads.github.com", EndpointType: params.GithubEndpointType}, CreatedAt: time.Unix(o.clock, 0)}
	f.repo.PoolManagerStatus.IsRunning = true
	f.scale = params.ScaleSet{RunnerPrefix: f.plan.Scale.RunnerPrefix, CreatedAt: time.Unix(o.clock, 0), ID: 27, ScaleSetID: 99, Name: f.plan.Scale.Name, DisableUpdate: true, State: params.ScaleSetCreated, ProviderName: "halo-lxd", MaxRunners: 1, Image: "halo-image", Flavor: "halo-ci", OSType: "linux", OSArch: "amd64", RunnerBootstrapTimeout: 300, ExtraSpecs: extra, GitHubRunnerGroup: "Default", RepoID: f.repo.ID, TemplateID: id}
	f.executor = connectionExecutor{files: o.issuer.files, parent: o.issuer.parent, producer: o.issuer.producer, now: o.issuer.now}
	f.executor.admit = func(ctx context.Context, p connectionPlan, s *serviceFiles, consume func(connectionAdmission) error) error {
		other, err := s.sibling("session")
		if err != nil {
			return err
		}
		defer other.close()
		credential, err := readManagedGARM(other, p.Source.SessionProducer, time.Unix(o.clock, 0))
		if err != nil {
			return err
		}
		defer credential.close()
		if competing, err := openServiceFiles(o.issuer.files, o.issuer.parent, "connection", "session.lock", false); err == nil {
			competing.close()
			t.Fatal("connection lease downgraded")
		}
		ad := connectionAdmission{app: appAdmission{key: o.key, origin: observerOrigin{bytesSHA(other.raw["complete.json"]), strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 32), credential.expires}, token: credential.token}, control: bus, boot: bus.boot, manager: bus.manager, controller: bus.before, check: func(context.Context) error { return nil }, webhook: func() (string, error) { return "synthetic webhook input with enough entropy", nil }}
		return consume(ad)
	}
	f.executor.recoverStop = func(ctx context.Context, s *serviceFiles, p connectionPlan, original connectionIntent, step string) error {
		return stopConnectionController(ctx, s, p, original, step, bus, o.issuer.now)
	}
	f.executor.fixture = sessionRoundTripFunc(func(r *http.Request) (*http.Response, error) { return f.respond(t, r) })
	f.encode(t)
	return f
}

func (f *connectionFixture) encode(t *testing.T) {
	t.Helper()
	var err error
	f.raw, err = json.Marshal(f.plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeConnectionPlan(f.raw); err != nil {
		t.Fatal("fixture plan", err)
	}
}

func (f *connectionFixture) respond(t *testing.T, r *http.Request) (*http.Response, error) {
	t.Helper()
	f.requests++
	status := 200
	var body any
	if r.Method == "POST" {
		f.posts[r.URL.Host+r.URL.Path]++
	}
	switch {
	case r.URL.Host == "api.github.com" && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		raw, _ := io.ReadAll(r.Body)
		var opts struct {
			IDs         []int64           `json:"repository_ids"`
			Permissions map[string]string `json:"permissions"`
		}
		if json.Unmarshal(raw, &opts) != nil {
			t.Fatal("token options")
		}
		if len(opts.IDs) == 0 {
			return f.observer.respond(t, rWithBody(r, raw))
		}
		if !reflect.DeepEqual(opts.IDs, []int64{1432619931}) || !reflect.DeepEqual(opts.Permissions, map[string]string{"metadata": "read", "administration": "write"}) {
			t.Fatal("admin token scope")
		}
		status = 201
		body = map[string]any{"token": "fixture-admin" + strings.Repeat("x", 160), "expires_at": time.Unix(f.observer.clock+3600, 0).UTC(), "permissions": opts.Permissions, "repository_selection": "selected"}
	case r.URL.Host == "api.github.com" && r.URL.Path == "/repos/headironc/nucleus/actions/runners/registration-token":
		if r.Method != "POST" {
			t.Fatal("registration method")
		}
		status = 201
		body = map[string]any{"token": "fixture-registration" + strings.Repeat("x", 100), "expires_at": time.Unix(f.observer.clock+3600, 0).UTC()}
	case r.URL.Host == "api.github.com":
		return f.observer.respond(t, r)
	case r.URL.Host == "github.com":
		raw, _ := io.ReadAll(r.Body)
		var opts params.ActionsServiceAdminInfoRequest
		if r.Method != "POST" || r.URL.Path != "/actions/runner-registration" || !strings.HasPrefix(r.Header.Get("Authorization"), "RemoteAuth fixture-registration") || json.Unmarshal(raw, &opts) != nil || opts.URL != "https://github.com/headironc/nucleus" || opts.RunnerEvent != "register" {
			t.Fatal("RemoteAuth shape")
		}
		enc := base64.RawURLEncoding.EncodeToString
		body = map[string]any{"url": "https://fixture.actions.githubusercontent.com/org/project/", "token": enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, f.observer.clock+3600))) + "." + enc([]byte("synthetic signature"))}
	case strings.HasSuffix(r.URL.Host, ".actions.githubusercontent.com"):
		if r.Method != "GET" || r.URL.Path != "/org/project/_apis/runtime/runnerscalesets" || r.URL.Query().Get("name") != f.plan.Scale.Name || r.URL.Query().Get("runnerGroupId") != "1" {
			t.Fatal("Actions read shape")
		}
		if f.observer.clock >= 1863129600+3600 {
			status = 401
			body = map[string]string{"message": "expired"}
		} else if !f.createdScale {
			body = map[string]any{"count": 0, "value": []any{}}
		} else {
			body = map[string]any{"count": 1, "value": []any{map[string]any{"id": 99, "name": f.plan.Scale.Name, "runnerGroupId": 1, "enabled": false, "RunnerSetting": map[string]bool{"ephemeral": true, "disableUpdate": true}, "createdOn": time.Unix(f.observer.clock, 0).UTC(), "labels": []any{map[string]string{"name": f.plan.Scale.Name, "type": "System"}}}}}
		}
	case r.URL.Host == "127.0.0.1:9997":
		if r.URL.Scheme != "http" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Fatal("GARM transport")
		}
		switch r.URL.Path {
		case "/api/v1/repositories":
			if r.Method == "POST" {
				var opts params.CreateRepoParams
				raw, _ := io.ReadAll(r.Body)
				if json.Unmarshal(raw, &opts) != nil || opts.Owner != "headironc" || opts.Name != "nucleus" || opts.CredentialsName != "nucleiron" || opts.WebhookSecret != "synthetic webhook input with enough entropy" || opts.AgentMode {
					t.Fatal("repository request")
				}
				f.createdRepo = true
				body = f.repo
			} else if f.createdRepo {
				body = []any{f.repo}
			} else {
				body = []any{}
			}
		case "/api/v1/repositories/" + f.repo.ID:
			body = f.repo
		case "/api/v1/repositories/" + f.repo.ID + "/scalesets":
			if r.Method != "POST" {
				t.Fatal("scale method")
			}
			raw, _ := io.ReadAll(r.Body)
			var opts params.CreateScaleSetParams
			if json.Unmarshal(raw, &opts) != nil || !reflect.DeepEqual(opts, f.plan.Scale) {
				t.Fatal("scaleset request")
			}
			f.createdScale = true
			body = f.scale
		case "/api/v1/scalesets":
			if f.createdScale {
				body = []any{f.scale}
			} else {
				body = []any{}
			}
		case "/api/v1/scalesets/27":
			body = f.scale
		case "/api/v1/pools", "/api/v1/instances":
			body = []any{}
		case "/api/v1/templates/7":
			body = params.Template{ID: 7, Owner: "system", OSType: "linux", ForgeType: params.GithubEndpointType, Data: []byte("synthetic system template")}
		case "/api/v1/github/credentials/1":
			body = map[string]any{"id": 1, "name": "nucleiron", "auth-type": "app", "forge_type": "github", "endpoint": f.repo.Endpoint, "updated_at": time.Unix(f.observer.clock, 0).UTC()}
		default:
			t.Fatalf("unexpected GARM path %s", r.URL.Path)
		}
	default:
		t.Fatalf("unexpected connection request %s %s", r.Method, r.URL)
	}
	raw, _ := json.Marshal(body)
	response := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}, Request: r}
	if f.mutate != nil {
		if err := f.mutate(r, response); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func rWithBody(r *http.Request, raw []byte) *http.Request {
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	return r
}

func TestConnectionCreatesDisabledBindingAndLocalRecovery(t *testing.T) {
	f := connectionFixtureFor(t)
	binding, stage, err := f.executor.execute(context.Background(), f.raw)
	if err != nil {
		t.Fatalf("connection stage=%s error=%v calls=%v", stage, err, f.posts)
	}
	if binding.Enabled || binding.Repository != f.repo.ID || binding.ScaleSet != 27 || binding.GitHubScaleSet != 99 || f.observer.deletes != 2 || len(f.bus.sends) != 0 {
		t.Fatalf("binding=%+v deletes=%d stops=%v", binding, f.observer.deletes, f.bus.sends)
	}
	requests := f.requests
	f.executor.admit = func(context.Context, connectionPlan, *serviceFiles, func(connectionAdmission) error) error {
		t.Fatal("original invokes admission")
		return errRefused
	}
	again, stage, err := f.executor.execute(context.Background(), f.raw)
	if err != nil || stage != "original" || again != binding || f.requests != requests {
		t.Fatal("original result changed", stage, err)
	}
	lease, err := loadConnection(f.executor.files, f.executor.parent, f.executor.producer, time.Unix(f.observer.clock, 0))
	if err != nil {
		t.Fatal("consumer", err)
	}
	if _, err := f.executor.retire(context.Background(), f.raw); err == nil {
		t.Fatal("retirement accepted active consumer")
	}
	lease.close()
	state, err := f.executor.retire(context.Background(), f.raw)
	if err != nil || state.Registration != "protected_until_documented_expiry" || state.Actions != "protected_until_original_expiry" {
		t.Fatalf("pending retirement=%+v %v", state, err)
	}
	f.observer.clock += 3601
	state, err = f.executor.retire(context.Background(), f.raw)
	if err != nil || state.Registration != "documented_expiry_elapsed_rejection_unverified" || state.Actions != "expiry_rejection_observed" {
		t.Fatalf("expiry=%+v %v", state, err)
	}
	requests = f.requests
	if _, err := f.executor.retire(context.Background(), f.raw); err != nil || f.requests != requests {
		t.Fatal("retirement repeated network", err)
	}
}

func TestConnectionUnknownCreateStopsOriginalControllerOnce(t *testing.T) {
	for _, step := range []string{"repository", "scaleset"} {
		t.Run(step, func(t *testing.T) {
			f := connectionFixtureFor(t)
			f.mutate = func(r *http.Request, _ *http.Response) error {
				if r.Method == "POST" && r.URL.Host == "127.0.0.1:9997" && (step == "repository" && r.URL.Path == "/api/v1/repositories" || step == "scaleset" && strings.HasSuffix(r.URL.Path, "/scalesets")) {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil {
				t.Fatal("unknown creation accepted")
			}
			if !reflect.DeepEqual(f.bus.sends, []string{"stop"}) {
				t.Fatal("original controller stop", f.bus.sends)
			}
			requests := f.requests
			if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil || f.requests != requests || !reflect.DeepEqual(f.bus.sends, []string{"stop"}) {
				t.Fatal("unknown create replayed")
			}
			if _, err := os.Stat(filepath.Join(f.executor.parent, "connection", "connection.json")); !os.IsNotExist(err) {
				t.Fatal("unknown produced binding")
			}
		})
	}
}

func TestConnectionStopRecoveryNeverResendsStop(t *testing.T) {
	for _, failure := range []string{"reply", "event", "wrong-event", "drain"} {
		t.Run(failure, func(t *testing.T) {
			f := connectionFixtureFor(t)
			switch failure {
			case "reply":
				f.bus.lostReply = "stop"
			case "event":
				f.bus.lostEvent = "stop"
			case "wrong-event":
				f.bus.badEvent = "stop"
			case "drain":
				f.bus.drainFailure = "controller"
			}
			f.mutate = func(r *http.Request, _ *http.Response) error {
				if r.Method == "POST" && r.URL.Host == "127.0.0.1:9997" {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil {
				t.Fatal("unknown accepted")
			}
			f.bus.lostReply = ""
			f.bus.lostEvent = ""
			f.bus.badEvent = ""
			f.bus.drainFailure = ""
			_, stage, err := f.executor.execute(context.Background(), f.raw)
			if err == nil || !reflect.DeepEqual(f.bus.sends, []string{"stop"}) {
				t.Fatal("stop resent", stage, err, f.bus.sends)
			}
			if failure != "reply" && stage != "original_create_unknown_controller_stopped" {
				t.Fatal("original job not recovered", stage)
			}
		})
	}
}

func TestConnectionSealedStopRequiresCurrentDrain(t *testing.T) {
	for _, kind := range []string{"restarted", "residual"} {
		t.Run(kind, func(t *testing.T) {
			f := connectionFixtureFor(t)
			f.mutate = func(r *http.Request, _ *http.Response) error {
				if r.Method == "POST" && r.URL.Host == "127.0.0.1:9997" {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil || !f.bus.stopped {
				t.Fatal("unknown creation not stopped")
			}
			if kind == "restarted" {
				f.bus.started = true
			} else {
				f.bus.drainFailure = "controller"
			}
			requests := f.requests
			if _, stage, err := f.executor.execute(context.Background(), f.raw); err == nil || stage != "original_stop_unknown" || f.requests != requests || !reflect.DeepEqual(f.bus.sends, []string{"stop"}) {
				t.Fatal("historical stop hid current resources", stage, err)
			}
		})
	}
}

func TestConnectionRetirementPreventsUnfinishedCreation(t *testing.T) {
	f := connectionFixtureFor(t)
	f.mutate = func(r *http.Request, _ *http.Response) error {
		if r.URL.Path == "/api/v1/templates/7" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil {
		t.Fatal("template failure accepted")
	}
	if _, err := f.executor.retire(context.Background(), f.raw); err != nil {
		t.Fatal(err)
	}
	requests := f.requests
	if _, stage, err := f.executor.execute(context.Background(), f.raw); err == nil || stage != "retirement_started" || f.requests != requests {
		t.Fatal("retired transaction resumed", stage, err)
	}
}

func TestConnectionExistingRepositoryNeedsAcceptedIdentity(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint(wrong), func(t *testing.T) {
			f := connectionFixtureFor(t)
			f.createdRepo = true
			f.plan.RepositoryMode = "existing"
			f.plan.Repository = f.repo.ID
			f.plan.RepositorySHA = connectionRepositoryIdentity(f.repo)
			if wrong {
				f.plan.RepositorySHA = strings.Repeat("9", 64)
			}
			f.encode(t)
			binding, stage, err := f.executor.execute(context.Background(), f.raw)
			if wrong {
				if err == nil || f.createdScale {
					t.Fatal("unaccepted repository promoted", stage)
				}
				return
			}
			if err != nil || binding.Repository != f.repo.ID || f.posts["127.0.0.1:9997/api/v1/repositories"] != 0 {
				t.Fatal("existing repository recreated", stage, err)
			}
		})
	}
}

func TestConnectionKnownCreateAndPartialPublicationRecovery(t *testing.T) {
	for _, failure := range []string{"read", "publication"} {
		t.Run(failure, func(t *testing.T) {
			f := connectionFixtureFor(t)
			if failure == "read" {
				f.mutate = func(r *http.Request, _ *http.Response) error {
					if r.URL.Path == "/api/v1/scalesets/27" {
						return io.ErrUnexpectedEOF
					}
					return nil
				}
			} else {
				failed := false
				f.executor.files.syncHook = func(file *os.File) error {
					if file.Name() == filepath.Join(f.executor.parent, "connection", "verified.json") && !failed {
						failed = true
						return io.ErrUnexpectedEOF
					}
					return file.Sync()
				}
			}
			if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil {
				t.Fatal("fault not observed")
			}
			if len(f.bus.sends) != 0 {
				t.Fatal("known create stopped controller")
			}
			posts := fmt.Sprint(f.posts)
			f.mutate = nil
			f.executor.files = f.observer.issuer.files
			binding, stage, err := f.executor.execute(context.Background(), f.raw)
			if err != nil || binding.GitHubScaleSet != 99 || fmt.Sprint(f.posts) != posts {
				t.Fatal("known create replayed", stage, err, "before", posts, "after", f.posts, "binding", binding)
			}
		})
	}
}

func TestConnectionRejectsReplacedIDsAndEnabledResources(t *testing.T) {
	for _, kind := range []string{"local-id", "remote-id", "enabled", "instance", "null-enabled", "foreign-repository"} {
		t.Run(kind, func(t *testing.T) {
			f := connectionFixtureFor(t)
			f.mutate = func(r *http.Request, response *http.Response) error {
				var body any
				if r.URL.Path == "/api/v1/scalesets/27" {
					s := f.scale
					switch kind {
					case "local-id":
						s.ScaleSetID = 100
					case "enabled":
						s.Enabled = true
					case "foreign-repository":
						s.RepoID = "dddddddd-0000-4000-8000-000000000004"
					case "null-enabled":
						raw, _ := json.Marshal(s)
						var wire map[string]any
						json.Unmarshal(raw, &wire)
						wire["enabled"] = nil
						body = wire
					default:
						return nil
					}
					if body == nil {
						body = s
					}
				} else if kind == "instance" && f.createdScale && r.URL.Path == "/api/v1/instances" {
					body = []any{map[string]any{"id": "unassigned-instance"}}
				} else if kind == "remote-id" && strings.HasSuffix(r.URL.Host, ".actions.githubusercontent.com") && f.createdScale {
					raw, _ := io.ReadAll(response.Body)
					response.Body = io.NopCloser(strings.NewReader(strings.Replace(string(raw), `"id":99`, `"id":100`, 1)))
					return nil
				} else if kind == "null-enabled" && r.URL.Path == "/api/v1/scalesets/27" {
					raw, _ := io.ReadAll(response.Body)
					var wire map[string]any
					json.Unmarshal(raw, &wire)
					wire["enabled"] = nil
					body = wire
				} else {
					return nil
				}
				raw, _ := json.Marshal(body)
				response.Body = io.NopCloser(strings.NewReader(string(raw)))
				return nil
			}
			if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil {
				t.Fatal("unsafe observation exported")
			}
			if len(f.bus.sends) != 0 {
				t.Fatal("observation failure classified as unknown create")
			}
		})
	}
}

func TestConnectionConsumerBindingAndReceiptIntegrity(t *testing.T) {
	f := connectionFixtureFor(t)
	if _, _, err := f.executor.execute(context.Background(), f.raw); err != nil {
		t.Fatal(err)
	}
	lease, err := loadConnection(f.executor.files, f.executor.parent, f.executor.producer, time.Unix(f.observer.clock, 0))
	if err != nil {
		t.Fatal(err)
	}
	r := request{RepositoryID: 1432619931, Owner: "headironc", Repository: "nucleus", Controller: f.plan.Source.Controller, GARMRepository: f.repo.ID, ScaleSet: 27}
	b := provider.ExecutionBinding{Controller: r.Controller, Repository: f.repo.ID, ScaleSet: f.plan.Scale.Name, Pool: f.plan.Scale.Name + "-" + f.repo.ID, Image: f.plan.ImageSHA}
	if !lease.matches(r, b) {
		t.Fatal("real consumer rejects binding")
	}
	b.Image = strings.Repeat("9", 64)
	if lease.matches(r, b) {
		t.Fatal("consumer accepts replacement image")
	}
	lease.close()
	path := filepath.Join(f.executor.parent, "connection", "scaleset-create.response.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := loadConnection(f.executor.files, f.executor.parent, f.executor.producer, time.Unix(f.observer.clock, 0)); err == nil {
		c.close()
		t.Fatal("changed original accepted")
	}
}

func TestConnectionImageUsesPrivateLXDRead(t *testing.T) {
	for _, kind := range []string{"valid", "public", "container", "alias"} {
		t.Run(kind, func(t *testing.T) {
			p := connectionPlan{ImageSHA: strings.Repeat("4", 64), Scale: params.CreateScaleSetParams{Image: "halo-image"}}
			bodies := machineBodies()
			imageType := "virtual-machine"
			if kind == "container" {
				imageType = "container"
			}
			bodies["/1.0/images/"+p.ImageSHA] = fmt.Sprintf(`{"fingerprint":%q,"type":%q,"public":%t}`, p.ImageSHA, imageType, kind == "public")
			target := p.ImageSHA
			if kind == "alias" {
				target = strings.Repeat("5", 64)
			}
			bodies["/1.0/images/aliases/halo-image"] = fmt.Sprintf(`{"name":"halo-image","target":%q,"type":"virtual-machine"}`, target)
			err := connectionImage(context.Background(), machineFixture(t, bodies), p)
			if (err == nil) != (kind == "valid") {
				t.Fatal("image boundary", kind, err)
			}
		})
	}
}

func TestConnectionCredentialFailuresNeverRepeatIssuance(t *testing.T) {
	for _, kind := range []string{"admin-scope", "admin-expiry", "registration-expiry", "registration-lost", "remote-auth-lost"} {
		t.Run(kind, func(t *testing.T) {
			f := connectionFixtureFor(t)
			f.mutate = func(r *http.Request, response *http.Response) error {
				if kind == "admin-expiry" && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/access_tokens") {
					raw, _ := io.ReadAll(response.Body)
					var wire map[string]any
					json.Unmarshal(raw, &wire)
					if strings.HasPrefix(fmt.Sprint(wire["token"]), "fixture-admin") {
						wire["expires_at"] = "invalid"
					}
					raw, _ = json.Marshal(wire)
					response.Body = io.NopCloser(strings.NewReader(string(raw)))
				}
				if kind == "admin-scope" && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/access_tokens") {
					raw, _ := io.ReadAll(response.Body)
					raw = []byte(strings.Replace(string(raw), `"administration":"write"`, `"administration":"read"`, 1))
					response.Body = io.NopCloser(strings.NewReader(string(raw)))
				}
				if strings.HasSuffix(r.URL.Path, "/registration-token") {
					if kind == "registration-lost" {
						return io.ErrUnexpectedEOF
					}
					if kind == "registration-expiry" {
						raw, _ := json.Marshal(map[string]any{"token": "synthetic-registration", "expires_at": time.Unix(f.observer.clock+60, 0).UTC()})
						response.Body = io.NopCloser(strings.NewReader(string(raw)))
					}
				}
				if kind == "remote-auth-lost" && r.URL.Host == "github.com" {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil || f.createdRepo || len(f.bus.sends) != 0 {
				t.Fatal("bad credential admitted")
			}
			if f.observer.deletes != 2 {
				t.Fatal("known tokens not retired", f.observer.deletes)
			}
			posts := fmt.Sprint(f.posts)
			f.mutate = nil
			if _, _, err := f.executor.execute(context.Background(), f.raw); err == nil || fmt.Sprint(f.posts) != posts {
				t.Fatal("credential issuance repeated")
			}
		})
	}
}

func TestConnectionActionsURLAndExpiryProjection(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	token := enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{"exp":1863133200}`)) + "." + enc([]byte("synthetic signature"))
	for _, base := range []string{"https://fixture.actions.githubusercontent.com/org/project/", "https://actions.githubusercontent.com/org/", "https://fixture.actions.githubusercontent.com.evil.example/org/", "http://fixture.actions.githubusercontent.com/org/", "https://fixture.actions.githubusercontent.com/org/?x=1", "https://fixture.actions.githubusercontent.com/org/%2f/"} {
		raw, _ := json.Marshal(map[string]string{"url": base, "token": token})
		_, err := parseConnectionActions(raw, 1863129600, 1863131400)
		if (err == nil) != (base == "https://fixture.actions.githubusercontent.com/org/project/") {
			t.Fatal("Actions URL accepted incorrectly", base)
		}
	}
	for _, expiry := range []string{"null", "0", "1863133200.5", `"1863133200"`} {
		bad := enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{"exp":`+expiry+`}`)) + "." + enc([]byte("signature"))
		if _, err := connectionJWTExpiry(bad); err == nil {
			t.Fatal("malformed expiry accepted", expiry)
		}
	}
}
