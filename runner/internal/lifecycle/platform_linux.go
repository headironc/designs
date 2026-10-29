package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	garm "github.com/cloudbase/garm/client"
	"github.com/cloudbase/garm/client/controller_info"
	"github.com/cloudbase/garm/client/instances"
	"github.com/cloudbase/garm/client/repositories"
	"github.com/cloudbase/garm/client/scalesets"
	"github.com/cloudbase/garm/params"
	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/google/go-github/v84/github"
	"github.com/headironc/designs/runner/internal/provider"
)

// The actual SDKs run through this transport, including discovery/error paths.
// The optional base transport is private fixture injection; endpoint identities
// remain fixed even in tests. No redirect, proxy, POST or unbounded body passes.
type readTransport struct {
	base         http.RoundTripper
	scheme, host string
}

func readResponse(response *http.Response) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil || len(raw) > 2*1024*1024 {
		return nil, errRefused
	}
	return raw, nil
}

func (t readTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.URL.Scheme != t.scheme || req.URL.Host != t.host || req.URL.User != nil || req.URL.Fragment != "" {
		return nil, errRefused
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, errRefused
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errRefused
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 || unambiguous(raw) != nil {
		return nil, errRefused
	}
	// These SDK fields have non-pointer representations. Preserve their wire
	// absence rules before decoding: GitHub total_count is required, whereas
	// GARM v0.2.1 ScaleSet.Enabled uses omitempty for disabled ScaleSets.
	if t.host == "api.github.com" && strings.HasSuffix(req.URL.Path, "/actions/runners") {
		var wire struct {
			Total *int `json:"total_count"`
		}
		if json.Unmarshal(raw, &wire) != nil || wire.Total == nil {
			return nil, errRefused
		}
	}
	if t.host == "127.0.0.1:9997" && !garmIdentityPresent(req.URL.Path, raw) {
		return nil, errRefused
	}
	if t.host == "127.0.0.1:9997" && !admissionWire(req.URL.Path, raw) {
		return nil, errRefused
	}
	if t.host == "127.0.0.1:9997" && strings.HasPrefix(req.URL.Path, "/api/v1/scalesets/") && strings.Count(strings.TrimPrefix(req.URL.Path, "/api/v1/scalesets/"), "/") == 0 {
		var wire map[string]json.RawMessage
		if json.Unmarshal(raw, &wire) != nil {
			return nil, errRefused
		}
		for key, value := range wire {
			if foldJSONName(key) == foldJSONName("enabled") && string(value) != "true" && string(value) != "false" {
				return nil, errRefused
			}
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

// GARM's generated response payloads and identity fields are value types.
// Restore required wire presence before SDK decoding erases absent/null into
// zero values. The SDK still decodes values; the consumer checks exact binding.
type endpointPresence struct {
	Type *string `json:"endpoint_type"`
	Base *string `json:"base_url"`
	API  *string `json:"api_base_url"`
}

func (e *endpointPresence) complete() bool {
	return e != nil && e.Type != nil && e.Base != nil && e.API != nil
}

func garmIdentityPresent(path string, raw []byte) bool {
	switch {
	case path == "/api/v1/instances" || strings.HasPrefix(path, "/api/v1/scalesets/") && strings.HasSuffix(path, "/instances"):
		var rows []struct {
			ID         *string         `json:"id"`
			Name       *string         `json:"name"`
			Provider   *string         `json:"provider_name"`
			ProviderID json.RawMessage `json:"provider_id"`
			AgentID    json.RawMessage `json:"agent_id"`
			ScaleSet   json.RawMessage `json:"scale_set_id"`
			Pool       json.RawMessage `json:"pool_id"`
			Status     json.RawMessage `json:"status"`
			Runner     json.RawMessage `json:"runner_status"`
		}
		if json.Unmarshal(raw, &rows) != nil || rows == nil {
			return false
		}
		for _, row := range rows {
			if row.ID == nil || row.Name == nil || row.Provider == nil {
				return false
			}
			// v0.2.1 omits zero assignment fields for pending instances and
			// the unused ScaleSet/pool branch. Explicit null is not produced.
			for _, value := range []json.RawMessage{row.ProviderID, row.AgentID, row.ScaleSet, row.Pool, row.Status, row.Runner} {
				if string(value) == "null" {
					return false
				}
			}
		}
		return true
	case path == "/api/v1/controller-info":
		var v *struct {
			ID *string `json:"controller_id"`
		}
		return json.Unmarshal(raw, &v) == nil && v != nil && v.ID != nil
	case strings.HasPrefix(path, "/api/v1/repositories/") && !strings.Contains(strings.TrimPrefix(path, "/api/v1/repositories/"), "/"):
		var v *struct {
			ID       *string           `json:"id"`
			Owner    *string           `json:"owner"`
			Name     *string           `json:"name"`
			Endpoint *endpointPresence `json:"endpoint"`
		}
		return json.Unmarshal(raw, &v) == nil && v != nil && v.ID != nil && v.Owner != nil && v.Name != nil && v.Endpoint.complete()
	case strings.HasPrefix(path, "/api/v1/scalesets/") && !strings.Contains(strings.TrimPrefix(path, "/api/v1/scalesets/"), "/"):
		var v *struct {
			ID         *uint32           `json:"id"`
			GitHubID   *int              `json:"scale_set_id"`
			Repository *string           `json:"repo_id"`
			Name       *string           `json:"name"`
			Provider   *string           `json:"provider_name"`
			Endpoint   *endpointPresence `json:"endpoint"`
		}
		return json.Unmarshal(raw, &v) == nil && v != nil && v.ID != nil && v.GitHubID != nil && v.Repository != nil && v.Name != nil && v.Provider != nil && v.Endpoint.complete()
	default:
		return true
	}
}

type platforms struct {
	github     *github.Client
	garm       *garm.GarmAPI
	token      string
	transports []*http.Transport
}

func newPlatforms(gh, ga string, fixture http.RoundTripper) *platforms {
	return credentialPlatforms(gh, ga, fixture, nil, nil)
}

func credentialPlatforms(gh, ga string, fixture http.RoundTripper, observer, session *sessionCredential) *platforms {
	p := &platforms{token: ga}
	client := func(scheme, host string) *http.Client {
		base := fixture
		if base == nil {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			p.transports = append(p.transports, transport)
			base = transport
		}
		credential := session
		if host == "api.github.com" {
			credential = observer
		}
		var wire http.RoundTripper = readTransport{base, scheme, host}
		if credential != nil && credential.store != nil {
			wire = credentialTransport{base: wire, credential: credential, now: time.Now}
		}
		return &http.Client{Transport: wire, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused }}
	}
	p.github = github.NewClient(client("https", "api.github.com")).WithAuthToken(gh)
	rt := httptransport.NewWithClient("127.0.0.1:9997", "/api/v1", []string{"http"}, client("http", "127.0.0.1:9997"))
	rt.Debug = false
	rt.Consumers = map[string]runtime.Consumer{"application/json": runtime.ConsumerFunc(func(r io.Reader, target any) error { return json.NewDecoder(r).Decode(target) })}
	p.garm = garm.New(rt, nil)
	return p
}

func (p *platforms) close() {
	for _, t := range p.transports {
		t.CloseIdleConnections()
	}
}

func validRepository(repo *github.Repository, r request) bool {
	return repo != nil && repo.ID != nil && *repo.ID == r.RepositoryID && repo.Name != nil && *repo.Name == r.Repository && repo.Owner != nil && repo.Owner.Login != nil && *repo.Owner.Login == r.Owner && repo.FullName != nil && *repo.FullName == r.Owner+"/"+r.Repository
}

func publicEndpoint(endpoint params.ForgeEndpoint) bool {
	// v0.2.1 validates both URLs at endpoint creation and returns their stored
	// bytes for repository and ScaleSet. Empty values have no default here.
	if endpoint.EndpointType != params.GithubEndpointType {
		return false
	}
	for _, pair := range []struct{ raw, host string }{{endpoint.BaseURL, "github.com"}, {endpoint.APIBaseURL, "api.github.com"}} {
		u, err := url.Parse(pair.raw)
		if err != nil || u.Scheme != "https" || u.Host != pair.host || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.RawPath != "" {
			return false
		}
	}
	return true
}

func (p *platforms) observe(ctx context.Context, r request, b provider.ExecutionBinding) (int, error) {
	repo, _, err := p.github.Repositories.Get(ctx, r.Owner, r.Repository)
	if err != nil || !validRepository(repo, r) {
		return 0, errRefused
	}
	run, _, err := p.github.Actions.GetWorkflowRunAttempt(ctx, r.Owner, r.Repository, r.RunID, r.Attempt, nil)
	if err != nil || run == nil || run.ID == nil || *run.ID != r.RunID || run.RunAttempt == nil || *run.RunAttempt != r.Attempt || run.HeadSHA == nil || *run.HeadSHA != r.Commit || run.Status == nil || *run.Status != "in_progress" || !validRepository(run.Repository, r) {
		return 0, errRefused
	}
	job, _, err := p.github.Actions.GetWorkflowJobByID(ctx, r.Owner, r.Repository, r.JobID)
	if err != nil || job == nil || job.ID == nil || *job.ID != r.JobID || job.RunID == nil || *job.RunID != r.RunID || job.RunAttempt == nil || *job.RunAttempt != int64(r.Attempt) || job.HeadSHA == nil || *job.HeadSHA != r.Commit || job.Status == nil || *job.Status != "in_progress" || job.RunnerID == nil || *job.RunnerID <= 0 || *job.RunnerID != b.AgentID || job.RunnerName == nil || *job.RunnerName != b.Name {
		return 0, errRefused
	}
	auth := httptransport.BearerToken(p.token)
	controller, err := p.garm.ControllerInfo.ControllerInfo(controller_info.NewControllerInfoParamsWithContext(ctx), auth)
	if err != nil || controller == nil || controller.Payload.ControllerID.String() != r.Controller {
		return 0, errRefused
	}
	garmRepo, err := p.garm.Repositories.GetRepo(repositories.NewGetRepoParamsWithContext(ctx).WithRepoID(r.GARMRepository), auth)
	if err != nil || garmRepo == nil || garmRepo.Payload.ID != r.GARMRepository || garmRepo.Payload.Owner != r.Owner || garmRepo.Payload.Name != r.Repository || !publicEndpoint(garmRepo.Payload.Endpoint) {
		return 0, errRefused
	}
	scale, err := p.garm.Scalesets.GetScaleSet(scalesets.NewGetScaleSetParamsWithContext(ctx).WithScalesetID(strconv.FormatUint(uint64(r.ScaleSet), 10)), auth)
	if err != nil || scale == nil || uint64(scale.Payload.ID) != uint64(r.ScaleSet) || scale.Payload.ScaleSetID <= 0 || scale.Payload.RepoID != r.GARMRepository || scale.Payload.Name != b.ScaleSet || scale.Payload.ProviderName != "halo-lxd" || scale.Payload.Name+"-"+scale.Payload.RepoID != b.Pool || !publicEndpoint(scale.Payload.Endpoint) || scale.Payload.OrgID != "" || scale.Payload.EnterpriseID != "" {
		return 0, errRefused
	}
	instance, err := p.garm.Instances.GetInstance(instances.NewGetInstanceParamsWithContext(ctx).WithInstanceName(b.Name), auth)
	if err != nil || instance == nil || instance.Payload.ID != r.Instance || instance.Payload.Name != b.Name || instance.Payload.ProviderID != b.Name || instance.Payload.ProviderName != "halo-lxd" || uint64(instance.Payload.ScaleSetID) != uint64(r.ScaleSet) || instance.Payload.AgentID != b.AgentID || instance.Payload.Status != "running" || instance.Payload.RunnerStatus != "active" {
		return 0, errRefused
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return scale.Payload.ScaleSetID, nil
}
