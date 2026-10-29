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

	"github.com/cloudbase/garm/params"
	"github.com/google/go-github/v84/github"
)

func connectionClock(s *serviceFiles, now int64) error {
	if now <= 0 || !s.unchanged() {
		return errRefused
	}
	for name, raw := range s.raw {
		if name == "request.json" {
			continue
		}
		var times struct {
			Started  int64 `json:"started_at"`
			Observed int64 `json:"observed_at"`
		}
		if json.Unmarshal(raw, &times) != nil || times.Started > now || times.Observed > now {
			return errRefused
		}
	}
	return nil
}

func (a *connectionAPI) public(step, token, method, path string, body any) (*github.Client, error) {
	client, err := a.client(step, "https", "api.github.com", token, "Bearer", method, path, "", body)
	if err != nil {
		return nil, err
	}
	return github.NewClient(client).WithAuthToken(token), nil
}

func (a *connectionAPI) adminMint(ctx context.Context, jwt string) error {
	opts := &github.InstallationTokenOptions{RepositoryIDs: []int64{1432619931}, Permissions: &github.InstallationPermissions{Metadata: github.Ptr("read"), Administration: github.Ptr("write")}}
	c, err := a.public("admin-mint", jwt, "POST", "/app/installations/"+strconv.FormatInt(a.plan.Source.Installation, 10)+"/access_tokens", opts)
	if err != nil {
		return err
	}
	_, _, err = c.Apps.CreateInstallationToken(ctx, a.plan.Source.Installation, opts)
	if err != nil {
		return errRefused
	}
	return nil
}

func (a *connectionAPI) adminToken() (observerToken, error) {
	r, err := a.response("admin-mint")
	if err != nil {
		return observerToken{}, errRefused
	}
	token, err := connectionToken(r.Body)
	if err != nil {
		return token, errRefused
	}
	var wire struct {
		Permissions map[string]string `json:"permissions"`
		Selection   *string           `json:"repository_selection"`
	}
	if json.Unmarshal(r.Body, &wire) != nil || r.Status != 201 || token.expires <= r.Observed || token.expires > r.Observed+3600 || token.expires < a.plan.Source.RequiredUntil || wire.Selection == nil || *wire.Selection != "selected" || len(wire.Permissions) != 2 || wire.Permissions["metadata"] != "read" || wire.Permissions["administration"] != "write" {
		return token, errRefused
	}
	return token, nil
}

// A usable original token remains a retirement responsibility even when its
// expiry is malformed. Unknown expiry forbids the expiry-based fallback.
func connectionToken(raw []byte) (observerToken, error) {
	var wire struct {
		Token   string          `json:"token"`
		Expires json.RawMessage `json:"expires_at"`
	}
	if unambiguous(raw) != nil || json.Unmarshal(raw, &wire) != nil || !validToken(wire.Token) {
		return observerToken{}, errRefused
	}
	token := observerToken{token: wire.Token}
	var expires time.Time
	if len(wire.Expires) == 0 || string(wire.Expires) == "null" || json.Unmarshal(wire.Expires, &expires) != nil || expires.IsZero() {
		return token, errRefused
	}
	token.expires = expires.Unix()
	return token, nil
}

func (a *connectionAPI) registration(ctx context.Context, token string) (observerToken, error) {
	c, err := a.public("registration", token, "POST", "/repos/headironc/nucleus/actions/runners/registration-token", nil)
	if err != nil {
		return observerToken{}, err
	}
	_, _, err = c.Actions.CreateRegistrationToken(ctx, "headironc", "nucleus")
	r, e := a.response("registration")
	var wire struct {
		Token   *string    `json:"token"`
		Expires *time.Time `json:"expires_at"`
	}
	if err != nil || e != nil || r.Status != 201 || unambiguous(r.Body) != nil || json.Unmarshal(r.Body, &wire) != nil || wire.Token == nil || !validToken(*wire.Token) || wire.Expires == nil || wire.Expires.Unix() <= r.Observed || wire.Expires.Unix() > r.Observed+3600 || wire.Expires.Unix() < a.plan.Source.RequiredUntil {
		return observerToken{}, errRefused
	}
	return observerToken{*wire.Token, wire.Expires.Unix()}, nil
}

func (a *connectionAPI) remoteAuth(ctx context.Context, token string) (connectionActions, error) {
	body := params.ActionsServiceAdminInfoRequest{URL: "https://github.com/headironc/nucleus", RunnerEvent: "register"}
	c, err := a.client("remote-auth", "https", "github.com", token, "RemoteAuth", "POST", "/actions/runner-registration", "", body)
	if err != nil {
		return connectionActions{}, err
	}
	raw, _ := json.Marshal(body)
	raw = append(raw, '\n')
	req, err := http.NewRequestWithContext(ctx, "POST", "https://github.com/actions/runner-registration", nil)
	if err != nil {
		return connectionActions{}, errRefused
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	req.Header.Set("Authorization", "RemoteAuth "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.Do(req)
	if response != nil {
		response.Body.Close()
	}
	r, e := a.response("remote-auth")
	if err != nil || e != nil || r.Status != 200 {
		return connectionActions{}, errRefused
	}
	return parseConnectionActions(r.Body, r.Observed, a.plan.Source.RequiredUntil)
}

func (a *connectionAPI) actionsRead(ctx context.Context, actions connectionActions, step string) (observerResponse, error) {
	u, err := url.Parse(actions.URL)
	if err != nil {
		return observerResponse{}, errRefused
	}
	query := url.Values{"api-version": []string{"6.0-preview"}, "name": []string{a.plan.Scale.Name}, "runnerGroupId": []string{"1"}}.Encode()
	path := u.Path + "_apis/runtime/runnerscalesets"
	c, err := a.client(step, "https", u.Host, actions.Token, "Bearer", "GET", path, query, nil)
	if err != nil {
		return observerResponse{}, err
	}
	u.Path, u.RawQuery = path, query
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return observerResponse{}, errRefused
	}
	req.Header.Set("Authorization", "Bearer "+actions.Token)
	response, err := c.Do(req)
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		return observerResponse{}, errRefused
	}
	return a.response(step)
}

func (a *connectionAPI) acquireCredentials(ctx context.Context, ad appAdmission) (connectionActions, error) {
	s := a.store
	reader := sessionAcquirer{files: s.files}
	p := a.plan.Source
	var intent observerIntent
	if reader.absent(s, "sign.intent.json") {
		intent = observerIntent{Version: 1, Request: bytesSHA(s.raw["request.json"]), Producer: a.producer, Started: a.now().Unix(), Origin: ad.origin}
		if a.check(ctx) != nil || p.RequiredUntil > a.now().Unix()+3600 || s.create("sign.intent.json", intent) != nil {
			return connectionActions{}, errRefused
		}
		signed, err := signObserver(p, ad.key, intent.Started)
		if err != nil || s.create("sign.response.json", signed) != nil {
			return connectionActions{}, errRefused
		}
	}
	if _, err := reader.read(s, "sign.intent.json", &intent); err != nil || intent.Request != bytesSHA(s.raw["request.json"]) || intent.Producer != a.producer || intent.Origin.Session != ad.origin.Session || intent.Origin.Config != ad.origin.Config || intent.Origin.Invocation != ad.origin.Invocation || intent.Origin.GARMExpires != ad.origin.GARMExpires || intent.Started <= 0 || a.now().Unix() < intent.Started {
		return connectionActions{}, errRefused
	}
	var signed observerJWT
	if _, err := reader.read(s, "sign.response.json", &signed); err != nil || verifyObserverJWT(p, &ad.key.PublicKey, signed, intent.Started) != nil {
		return connectionActions{}, errRefused
	}
	base := a.fixture
	if base == nil {
		transport := observerBase()
		a.transports = append(a.transports, transport)
		base = transport
	}
	basic := func(c context.Context) error {
		if c.Err() != nil {
			return errRefused
		}
		return connectionClock(s, a.now().Unix())
	}
	api := observerAPI{store: s, plan: p, producer: a.producer, base: base, now: a.now, check: func(c context.Context) error {
		if a.now().Unix() >= signed.Expires || a.check(c) != nil {
			return errRefused
		}
		return nil
	}}
	retire := api
	retire.check = basic
	cleanupToken := func(name string, token observerToken) {
		if validToken(token.token) {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_, _ = retire.retire(cleanup, name, token)
		}
	}
	// A retained successful RemoteAuth response can resume after the local App
	// JWT expires. It never grants a replacement sign, mint or registration.
	if !reader.absent(s, "remote-auth.response.json") {
		r, err := a.response("remote-auth")
		if err != nil || r.Status != 200 {
			return connectionActions{}, errRefused
		}
		actions, err := parseConnectionActions(r.Body, r.Observed, p.RequiredUntil)
		if err != nil {
			return connectionActions{}, errRefused
		}
		admin, err := a.adminToken()
		if err != nil {
			return connectionActions{}, errRefused
		}
		if _, err := retire.retire(ctx, "admin", admin); err != nil {
			return connectionActions{}, errRefused
		}
		// Carry the original credential judgment into the final binding even
		// when this process only resumes the retained RemoteAuth response.
		for _, step := range []string{"installation-before", "installation-after", "audit-mint", "audit-repositories", "audit-delete", "audit-rejection", "admin-repositories", "registration"} {
			if _, err := a.response(step); err != nil {
				return connectionActions{}, errRefused
			}
		}
		return actions, nil
	}
	before, err := api.installation(ctx, "installation-before", signed.Token)
	if err != nil {
		return connectionActions{}, errRefused
	}
	mintErr := api.mint(ctx, "audit-mint", signed.Token, true)
	if response, err := a.response("audit-mint"); err == nil {
		known, _ := connectionToken(response.Body)
		defer cleanupToken("audit", known)
	}
	audit, err := api.minted("audit-mint", true)
	if mintErr != nil || err != nil {
		return connectionActions{}, errRefused
	}
	if api.repositories(ctx, "audit-repositories", audit.token) != nil {
		_, _ = retire.retire(ctx, "audit", audit)
		return connectionActions{}, errRefused
	}
	after, err := api.installation(ctx, "installation-after", signed.Token)
	if err != nil || before != after {
		_, _ = retire.retire(ctx, "audit", audit)
		return connectionActions{}, errRefused
	}
	if state, err := retire.retire(ctx, "audit", audit); err != nil || state != "revocation_rejection_observed" {
		return connectionActions{}, errRefused
	}
	if api.check(ctx) != nil {
		return connectionActions{}, errRefused
	}
	mintErr = a.adminMint(ctx, signed.Token)
	admin, err := a.adminToken()
	defer cleanupToken("admin", admin)
	if mintErr != nil || err != nil {
		return connectionActions{}, errRefused
	}
	if api.repositories(ctx, "admin-repositories", admin.token) != nil {
		return connectionActions{}, errRefused
	}
	registration, err := a.registration(ctx, admin.token)
	if err != nil {
		return connectionActions{}, errRefused
	}
	actions, err := a.remoteAuth(ctx, registration.token)
	if err != nil {
		return connectionActions{}, errRefused
	}
	if state, err := retire.retire(ctx, "admin", admin); err != nil || state != "revocation_rejection_observed" {
		return connectionActions{}, errRefused
	}
	return actions, nil
}
