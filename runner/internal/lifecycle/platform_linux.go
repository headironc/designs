package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

type platforms struct {
	github     *github.Client
	garm       *garm.GarmAPI
	token      string
	transports []*http.Transport
}

func newPlatforms(gh, ga string, fixture http.RoundTripper) *platforms {
	p := &platforms{token: ga}
	client := func(scheme, host string) *http.Client {
		base := fixture
		if base == nil {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			p.transports = append(p.transports, transport)
			base = transport
		}
		return &http.Client{Transport: readTransport{base, scheme, host}, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused }}
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
