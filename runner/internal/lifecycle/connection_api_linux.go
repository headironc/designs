package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	garm "github.com/cloudbase/garm/client"
	"github.com/cloudbase/garm/client/credentials"
	"github.com/cloudbase/garm/client/instances"
	"github.com/cloudbase/garm/client/pools"
	"github.com/cloudbase/garm/client/repositories"
	"github.com/cloudbase/garm/client/scalesets"
	"github.com/cloudbase/garm/client/templates"
	"github.com/cloudbase/garm/params"
	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
)

// Fresh read names never confer another POST/DELETE opportunity. Original
// write names remain fixed for the whole permanent connection slot.
type connectionAPI struct {
	store               *serviceFiles
	plan                connectionPlan
	producer, garmToken string
	now                 func() time.Time
	check               func(context.Context) error
	fixture             http.RoundTripper
	transports          []*http.Transport
}

func (a *connectionAPI) close() {
	for _, t := range a.transports {
		t.CloseIdleConnections()
	}
}

func (a *connectionAPI) nextRead(purpose string) (string, error) {
	entries, err := os.ReadDir(a.store.path)
	if err != nil || len(entries) > 10000 {
		return "", errRefused
	}
	max := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "read-") {
			parts := strings.Split(name, "-")
			if len(parts) < 3 || len(parts[1]) != 6 {
				return "", errRefused
			}
			n, err := strconv.Atoi(parts[1])
			if err != nil || n <= 0 {
				return "", errRefused
			}
			if n > max {
				max = n
			}
		}
	}
	if max >= 999999 {
		return "", errRefused
	}
	return fmt.Sprintf("read-%06d-%s", max+1, purpose), nil
}

func (a *connectionAPI) client(step, scheme, host, token, auth, method, path, query string, body any) (*http.Client, error) {
	if !validToken(token) {
		return nil, errRefused
	}
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil || len(raw) > 8191 {
			return nil, errRefused
		}
		raw = append(raw, '\n')
	}
	base := a.fixture
	if base == nil {
		transport := observerBase()
		if scheme == "http" && host == "127.0.0.1:9997" {
			transport.DialContext = func(c context.Context, network, address string) (net.Conn, error) {
				return dialSession(c, network, address, a.check)
			}
		}
		a.transports = append(a.transports, transport)
		base = transport
	}
	wire := &observerTransport{base: base, store: a.store, step: step, producer: a.producer, request: bytesSHA(a.store.raw["request.json"]), token: token, method: method, path: path, query: query, body: raw, now: a.now, check: a.check, scheme: scheme, host: host, authScheme: auth}
	return &http.Client{Transport: wire, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused }}, nil
}

func (a *connectionAPI) native(step, method, path, query string, body any) (*garm.GarmAPI, error) {
	client, err := a.client(step, "http", "127.0.0.1:9997", a.garmToken, "Bearer", method, "/api/v1"+path, query, body)
	if err != nil {
		return nil, err
	}
	rt := httptransport.NewWithClient("127.0.0.1:9997", "/api/v1", []string{"http"}, client)
	rt.Debug = false
	rt.Consumers = map[string]runtime.Consumer{"application/json": runtime.ConsumerFunc(func(r io.Reader, target any) error { return json.NewDecoder(r).Decode(target) })}
	return garm.New(rt, nil), nil
}

func (a *connectionAPI) response(step string) (observerResponse, error) {
	return (observerAPI{store: a.store, producer: a.producer}).response(step)
}

func (a *connectionAPI) createRepository(ctx context.Context, webhook string) (params.Repository, error) {
	body := params.CreateRepoParams{Owner: "headironc", Name: "nucleus", CredentialsName: a.plan.Source.CredentialName, WebhookSecret: webhook, ForgeType: params.GithubEndpointType}
	if body.Validate() != nil {
		return params.Repository{}, errRefused
	}
	client, err := a.native("repository-create", "POST", "/repositories", "", body)
	if err != nil {
		return params.Repository{}, err
	}
	_, err = client.Repositories.CreateRepo(repositories.NewCreateRepoParamsWithContext(ctx).WithBody(body), httptransport.BearerToken(a.garmToken))
	r, e := a.response("repository-create")
	if err != nil || e != nil || r.Status != 200 {
		return params.Repository{}, errRefused
	}
	return connectionRepository(r.Body, a.plan, false)
}

func (a *connectionAPI) createScale(ctx context.Context, repository string) (params.ScaleSet, error) {
	if !uuidPattern.MatchString(repository) {
		return params.ScaleSet{}, errRefused
	}
	client, err := a.native("scaleset-create", "POST", "/repositories/"+repository+"/scalesets", "", a.plan.Scale)
	if err != nil {
		return params.ScaleSet{}, err
	}
	_, err = client.Repositories.CreateRepoScaleSet(repositories.NewCreateRepoScaleSetParamsWithContext(ctx).WithRepoID(repository).WithBody(a.plan.Scale), httptransport.BearerToken(a.garmToken))
	r, e := a.response("scaleset-create")
	if err != nil || e != nil || r.Status != 200 {
		return params.ScaleSet{}, errRefused
	}
	return connectionScale(r.Body, a.plan, repository, false)
}

func (a *connectionAPI) repository(ctx context.Context, id string) (params.Repository, string, error) {
	if !uuidPattern.MatchString(id) {
		return params.Repository{}, "", errRefused
	}
	step, err := a.nextRead("repository")
	if err != nil {
		return params.Repository{}, "", err
	}
	client, err := a.native(step, "GET", "/repositories/"+id, "", nil)
	if err != nil {
		return params.Repository{}, "", err
	}
	_, err = client.Repositories.GetRepo(repositories.NewGetRepoParamsWithContext(ctx).WithRepoID(id), httptransport.BearerToken(a.garmToken))
	r, e := a.response(step)
	if err != nil || e != nil || r.Status != 200 {
		return params.Repository{}, "", errRefused
	}
	repo, err := connectionRepository(r.Body, a.plan, true)
	if err != nil || repo.ID != id {
		return params.Repository{}, "", errRefused
	}
	return repo, step, nil
}

func (a *connectionAPI) scale(ctx context.Context, id uint, repository string) (params.ScaleSet, string, error) {
	if id == 0 || uint64(id) > 4294967295 {
		return params.ScaleSet{}, "", errRefused
	}
	step, err := a.nextRead("scaleset")
	if err != nil {
		return params.ScaleSet{}, "", err
	}
	client, err := a.native(step, "GET", "/scalesets/"+strconv.FormatUint(uint64(id), 10), "", nil)
	if err != nil {
		return params.ScaleSet{}, "", err
	}
	_, err = client.Scalesets.GetScaleSet(scalesets.NewGetScaleSetParamsWithContext(ctx).WithScalesetID(strconv.FormatUint(uint64(id), 10)), httptransport.BearerToken(a.garmToken))
	r, e := a.response(step)
	if err != nil || e != nil || r.Status != 200 {
		return params.ScaleSet{}, "", errRefused
	}
	scale, err := connectionScale(r.Body, a.plan, repository, true)
	if err != nil || scale.ID != id {
		return params.ScaleSet{}, "", errRefused
	}
	return scale, step, nil
}

func (a *connectionAPI) template(ctx context.Context) error {
	step, err := a.nextRead("template")
	if err != nil {
		return err
	}
	id := *a.plan.Scale.TemplateID
	client, err := a.native(step, "GET", "/templates/"+strconv.FormatUint(uint64(id), 10), "", nil)
	if err != nil {
		return err
	}
	_, err = client.Templates.GetTemplate(templates.NewGetTemplateParamsWithContext(ctx).WithTemplateID(float64(id)), httptransport.BearerToken(a.garmToken))
	r, e := a.response(step)
	var template params.Template
	if err != nil || e != nil || r.Status != 200 || !connectionWire(r.Body, "id", "data", "owner_id", "os_type", "forge_type") || json.Unmarshal(r.Body, &template) != nil || template.ID != id || template.Owner != "system" || template.OSType != "linux" || template.ForgeType != params.GithubEndpointType || bytesSHA(template.Data) != a.plan.TemplateSHA {
		return errRefused
	}
	return nil
}

func (a *connectionAPI) credential(ctx context.Context) error {
	step, err := a.nextRead("credential")
	if err != nil {
		return err
	}
	id := a.plan.Source.Credential
	client, err := a.native(step, "GET", "/github/credentials/"+strconv.FormatInt(id, 10), "", nil)
	if err != nil {
		return err
	}
	_, err = client.Credentials.GetCredentials(credentials.NewGetCredentialsParamsWithContext(ctx).WithID(id), httptransport.BearerToken(a.garmToken))
	r, e := a.response(step)
	var value params.ForgeCredentials
	if err != nil || e != nil || r.Status != 200 || !connectionWire(r.Body, "id", "name", "auth-type", "forge_type", "endpoint", "updated_at") || json.Unmarshal(r.Body, &value) != nil || value.ID != uint(id) || value.Name != a.plan.Source.CredentialName || value.AuthType != params.ForgeAuthTypeApp || value.ForgeType != params.GithubEndpointType || value.Endpoint.Name != a.plan.Source.Endpoint || !publicEndpoint(value.Endpoint) {
		return errRefused
	}
	return nil
}

func (a *connectionAPI) inventory(ctx context.Context, repository string, scaleID uint) error {
	for _, path := range []string{"/repositories", "/scalesets", "/pools", "/instances"} {
		step, err := a.nextRead(strings.TrimPrefix(path, "/"))
		if err != nil {
			return err
		}
		client, err := a.native(step, "GET", path, "", nil)
		if err != nil {
			return err
		}
		auth := httptransport.BearerToken(a.garmToken)
		switch path {
		case "/repositories":
			_, err = client.Repositories.ListRepos(repositories.NewListReposParamsWithContext(ctx), auth)
		case "/scalesets":
			_, err = client.Scalesets.ListScalesets(scalesets.NewListScalesetsParamsWithContext(ctx), auth)
		case "/pools":
			_, err = client.Pools.ListPools(pools.NewListPoolsParamsWithContext(ctx), auth)
		case "/instances":
			_, err = client.Instances.ListInstances(instances.NewListInstancesParamsWithContext(ctx), auth)
		}
		r, e := a.response(step)
		var rows []json.RawMessage
		if err != nil || e != nil || r.Status != 200 || unambiguous(r.Body) != nil || json.Unmarshal(r.Body, &rows) != nil || rows == nil || len(rows) > 10000 {
			return errRefused
		}
		seen := map[string]bool{}
		foundRepo, foundScale := 0, 0
		for _, raw := range rows {
			switch path {
			case "/instances":
				return errRefused
			case "/repositories":
				var repo params.Repository
				if !connectionWire(raw, "id", "owner", "name", "endpoint") || json.Unmarshal(raw, &repo) != nil || !uuidPattern.MatchString(repo.ID) || seen[repo.ID] {
					return errRefused
				}
				seen[repo.ID] = true
				if repo.Owner == "headironc" && repo.Name == "nucleus" {
					if repository == "" || repo.ID != repository || repo.Endpoint.Name != a.plan.Source.Endpoint {
						return errRefused
					}
					foundRepo++
				}
			case "/scalesets":
				var scale params.ScaleSet
				if !connectionWire(raw, "id", "name") || json.Unmarshal(raw, &scale) != nil || scale.ID == 0 || uint64(scale.ID) > 4294967295 || scale.Enabled || len(scale.Instances) != 0 || scale.DesiredRunnerCount != 0 {
					return errRefused
				}
				id := strconv.FormatUint(uint64(scale.ID), 10)
				if seen[id] {
					return errRefused
				}
				seen[id] = true
				if repository != "" && scale.RepoID == repository || scale.Name == a.plan.Scale.Name {
					if scaleID == 0 || scale.ID != scaleID || scale.RepoID != repository {
						return errRefused
					}
					foundScale++
				}
			case "/pools":
				var pool params.Pool
				if !connectionWire(raw, "id") || json.Unmarshal(raw, &pool) != nil || !uuidPattern.MatchString(pool.ID) || pool.Enabled || len(pool.Instances) != 0 || seen[pool.ID] {
					return errRefused
				}
				seen[pool.ID] = true
			}
		}
		if path == "/repositories" && (repository == "" && foundRepo != 0 || repository != "" && foundRepo != 1) || path == "/scalesets" && (scaleID == 0 && foundScale != 0 || scaleID != 0 && foundScale != 1) {
			return errRefused
		}
	}
	return nil
}
