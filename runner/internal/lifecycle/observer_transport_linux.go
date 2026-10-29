package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/go-github/v84/github"
)

type observerResponse struct {
	Intent   string `json:"intent_sha256"`
	Status   int    `json:"status"`
	Body     []byte `json:"body"`
	Link     string `json:"link"`
	Observed int64  `json:"observed_at"`
}

type observerCall struct {
	Version       int    `json:"version"`
	Request       string `json:"request_sha256"`
	Producer      string `json:"producer_sha256"`
	Step          string `json:"step"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Query         string `json:"query"`
	Body          string `json:"body_sha256"`
	Authorization string `json:"authorization_sha256"`
	Started       int64  `json:"started_at"`
}

// One SDK invocation is bound to one persisted intent. Replaying a retained
// HTTP response uses the SDK decoder without repeating its network request.
type observerTransport struct {
	base                                                http.RoundTripper
	store                                               *serviceFiles
	step, producer, request, token, method, path, query string
	body                                                []byte
	now                                                 func() time.Time
	check                                               func(context.Context) error
	sent                                                atomic.Bool
}

func (t *observerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.sent.CompareAndSwap(false, true) || req.URL.Scheme != "https" || req.URL.Host != "api.github.com" || req.URL.User != nil || req.URL.Fragment != "" || req.URL.RawPath != "" || req.URL.ForceQuery || req.Method != t.method || req.URL.Path != t.path || req.URL.RawQuery != t.query || req.Header.Get("Authorization") != "Bearer "+t.token {
		return nil, errRefused
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(req.Body, 8193))
		req.Body.Close()
		if err != nil || len(body) > 8192 {
			return nil, errRefused
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	if !bytes.Equal(body, t.body) {
		return nil, errRefused
	}
	a := sessionAcquirer{files: t.store.files}
	intentName, responseName := t.step+".intent.json", t.step+".response.json"
	i := observerCall{1, t.request, t.producer, t.step, t.method, t.path, t.query, bytesSHA(body), bytesSHA([]byte(t.token)), t.now().Unix()}
	exists := !a.absent(t.store, intentName)
	if exists {
		var saved observerCall
		if _, err := a.read(t.store, intentName, &saved); err != nil {
			return nil, errRefused
		}
		i.Started = saved.Started
		if i != saved || i.Started <= 0 {
			return nil, errRefused
		}
	} else {
		if !a.absent(t.store, responseName) || t.check == nil || t.check(req.Context()) != nil || t.store.create(intentName, i) != nil {
			return nil, errRefused
		}
	}
	var result observerResponse
	if !a.absent(t.store, responseName) {
		if _, err := a.read(t.store, responseName, &result); err != nil {
			return nil, errRefused
		}
	} else {
		// An interrupted POST or DELETE is never sent again. GET recovery is
		// read-only and only needed when its original response was not retained.
		if exists && t.method != http.MethodGet || t.check == nil || t.check(req.Context()) != nil || t.now().Unix() < i.Started {
			return nil, errRefused
		}
		req.GetBody = nil
		if t.base == nil {
			return nil, errRefused
		}
		response, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, errRefused
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 24*1024+1))
		if err != nil || len(raw) > 24*1024 || len(response.Header.Values("Link")) > 1 || len(response.Header.Get("Link")) > 2048 {
			return nil, errRefused
		}
		result = observerResponse{bytesSHA(t.store.raw[intentName]), response.StatusCode, raw, response.Header.Get("Link"), t.now().Unix()}
		if result.Observed < i.Started || !t.store.unchanged() || t.store.create(responseName, result) != nil {
			return nil, errRefused
		}
	}
	if result.Intent != bytesSHA(t.store.raw[intentName]) || result.Status < 100 || result.Status > 599 || result.Observed < i.Started || t.store.files.syncFile(t.store.dir) != nil || !t.store.unchanged() {
		return nil, errRefused
	}
	return &http.Response{StatusCode: result.Status, Header: http.Header{"Link": []string{result.Link}, "Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(result.Body)), Request: req}, nil
}

type observerAPI struct {
	store    *serviceFiles
	plan     observerPlan
	producer string
	issuer   string
	base     http.RoundTripper
	now      func() time.Time
	check    func(context.Context) error
}

func (a observerAPI) client(step, token, method, path, query string, body any) (*github.Client, error) {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, errRefused
		}
		raw = append(raw, '\n')
	}
	if !validToken(token) {
		return nil, errRefused
	}
	producer := a.producer
	reader := sessionAcquirer{files: a.store.files}
	if !reader.absent(a.store, step+".intent.json") {
		var saved observerCall
		if _, err := reader.read(a.store, step+".intent.json", &saved); err != nil || saved.Producer != a.producer && (a.issuer == "" || saved.Producer != a.issuer) {
			return nil, errRefused
		}
		producer = saved.Producer
	}
	t := &observerTransport{base: a.base, store: a.store, step: step, producer: producer, request: bytesSHA(a.store.raw["request.json"]), token: token, method: method, path: path, query: query, body: raw, now: a.now, check: a.check}
	client := github.NewClient(&http.Client{Transport: t, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused }}).WithAuthToken(token)
	return client, nil
}

func (a observerAPI) installation(ctx context.Context, step, token string) (string, error) {
	c, err := a.client(step, token, http.MethodGet, "/app/installations/"+strconv.FormatInt(a.plan.Installation, 10), "", nil)
	if err != nil {
		return "", err
	}
	i, _, err := c.Apps.GetInstallation(ctx, a.plan.Installation)
	response, e := a.response(step)
	var wire map[string]json.RawMessage
	if err != nil || e != nil || response.Status != 200 || unambiguous(response.Body) != nil || json.Unmarshal(response.Body, &wire) != nil || wire["suspended_at"] == nil || string(wire["suspended_at"]) != "null" || i.GetID() != a.plan.Installation || i.GetAppID() != a.plan.App || i.Account.GetID() != a.plan.Account || i.Account.GetLogin() != "headironc" || i.GetRepositorySelection() != "selected" || i.SuspendedBy != nil || i.Permissions == nil {
		return "", errRefused
	}
	permissions, _ := json.Marshal(i.Permissions)
	var available map[string]string
	if json.Unmarshal(permissions, &available) != nil || available["metadata"] != "read" || available["actions"] != "read" && available["actions"] != "write" || available["administration"] != "read" && available["administration"] != "write" {
		return "", errRefused
	}
	// Bind the full installation response. Unknown server fields are retained
	// and participate in drift detection, rather than discarded by the SDK.
	return bytesSHA(response.Body), nil
}

func (a observerAPI) response(step string) (observerResponse, error) {
	var r observerResponse
	reader := sessionAcquirer{files: a.store.files}
	var i observerCall
	if _, err := reader.read(a.store, step+".intent.json", &i); err != nil || i.Version != 1 || i.Request != bytesSHA(a.store.raw["request.json"]) || i.Step != step || i.Started <= 0 || i.Producer != a.producer && (a.issuer == "" || i.Producer != a.issuer) {
		return r, errRefused
	}
	if _, err := reader.read(a.store, step+".response.json", &r); err != nil || r.Intent != bytesSHA(a.store.raw[step+".intent.json"]) || r.Observed < i.Started {
		return observerResponse{}, errRefused
	}
	return r, nil
}

func (a observerAPI) mint(ctx context.Context, step, jwt string, audit bool) error {
	permissions := &github.InstallationPermissions{Metadata: github.Ptr("read")}
	opts := &github.InstallationTokenOptions{Permissions: permissions}
	if !audit {
		opts.RepositoryIDs = []int64{1432619931}
		permissions.Actions = github.Ptr("read")
		permissions.Administration = github.Ptr("read")
	}
	c, err := a.client(step, jwt, http.MethodPost, "/app/installations/"+strconv.FormatInt(a.plan.Installation, 10)+"/access_tokens", "", opts)
	if err != nil {
		return err
	}
	_, _, err = c.Apps.CreateInstallationToken(ctx, a.plan.Installation, opts)
	if err != nil {
		return errRefused
	}
	return nil
}

type observerToken struct {
	token   string
	expires int64
}

func (a observerAPI) minted(step string, audit bool) (observerToken, error) {
	r, err := a.response(step)
	var wire struct {
		Token       *string         `json:"token"`
		Expires     *time.Time      `json:"expires_at"`
		Permissions json.RawMessage `json:"permissions"`
		Selection   *string         `json:"repository_selection"`
	}
	if err != nil || unambiguous(r.Body) != nil || json.Unmarshal(r.Body, &wire) != nil || wire.Token == nil || !validToken(*wire.Token) {
		return observerToken{}, errRefused
	}
	t := observerToken{token: *wire.Token}
	if wire.Expires != nil {
		t.expires = wire.Expires.Unix()
	}
	if r.Status != 201 || wire.Expires == nil {
		return t, errRefused
	}
	if !exactObserverPermissions(wire.Permissions, audit) || wire.Selection == nil || *wire.Selection != "selected" || t.expires <= r.Observed || t.expires > r.Observed+3600 {
		return t, errRefused
	}
	return t, nil
}

func (a observerAPI) repositories(ctx context.Context, step, token string) error {
	c, err := a.client(step, token, http.MethodGet, "/installation/repositories", "page=1&per_page=100", nil)
	if err != nil {
		return err
	}
	repos, resp, err := c.Apps.ListRepos(ctx, &github.ListOptions{Page: 1, PerPage: 100})
	r, e := a.response(step)
	if err != nil || e != nil || r.Status != 200 || unambiguous(r.Body) != nil || repos == nil || repos.TotalCount == nil || *repos.TotalCount != 1 || len(repos.Repositories) != 1 || resp.NextPage != 0 || r.Link != "" {
		return errRefused
	}
	repo := repos.Repositories[0]
	if repo == nil || repo.GetID() != 1432619931 || repo.GetFullName() != "headironc/nucleus" || repo.GetName() != "nucleus" || repo.Owner.GetID() != a.plan.Account || repo.Owner.GetLogin() != "headironc" {
		return errRefused
	}
	return nil
}

// A lost DELETE receipt can only complete via actual expiry and rejection.
// A bare 401 before expiry never upgrades an unknown DELETE to revoked.
func (a observerAPI) retire(ctx context.Context, step string, t observerToken) (string, error) {
	reader := sessionAcquirer{files: a.store.files}
	if !reader.absent(a.store, step+"-delete.intent.json") {
		var i observerCall
		if _, err := reader.read(a.store, step+"-delete.intent.json", &i); err != nil || i.Version != 1 || i.Request != bytesSHA(a.store.raw["request.json"]) || i.Step != step+"-delete" || i.Method != "DELETE" || i.Path != "/installation/token" || i.Query != "" || i.Body != bytesSHA(nil) || i.Authorization != bytesSHA([]byte(t.token)) || i.Started <= 0 || i.Producer != a.producer && (a.issuer == "" || i.Producer != a.issuer) {
			return "", errRefused
		}
	}
	if reader.absent(a.store, step+"-delete.intent.json") || !reader.absent(a.store, step+"-delete.response.json") {
		c, err := a.client(step+"-delete", t.token, http.MethodDelete, "/installation/token", "", nil)
		if err != nil {
			return "", err
		}
		_, _ = c.Apps.RevokeInstallationToken(ctx)
	}
	deleted := false
	if !reader.absent(a.store, step+"-delete.response.json") {
		r, err := a.response(step + "-delete")
		if err != nil {
			return "", err
		}
		deleted = r.Status == 204 && len(r.Body) == 0
	}
	if !deleted && (t.expires <= 0 || a.now().Unix() < t.expires) {
		return "", errRefused
	}
	c, err := a.client(step+"-rejection", t.token, http.MethodGet, "/installation/repositories", "page=1&per_page=100", nil)
	if err != nil {
		return "", err
	}
	_, _, _ = c.Apps.ListRepos(ctx, &github.ListOptions{Page: 1, PerPage: 100})
	r, err := a.response(step + "-rejection")
	if err != nil || r.Status != 401 || r.Observed <= 0 || !deleted && r.Observed < t.expires {
		return "", errRefused
	}
	if deleted {
		return "revocation_rejection_observed", nil
	}
	return "expiry_rejection_observed", nil
}
