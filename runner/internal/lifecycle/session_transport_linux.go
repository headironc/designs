package lifecycle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	garm "github.com/cloudbase/garm/client"
	"github.com/cloudbase/garm/client/controller_info"
	"github.com/cloudbase/garm/client/login"
	"github.com/cloudbase/garm/params"
	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
)

// sessionTransport is private to one persisted login intent. Retaining the
// original response precedes SDK decoding, including for a non-success status.
// Its caller must durably reserve the global slot before constructing it.
type sessionTransport struct {
	base   http.RoundTripper
	retain func(int, []byte) error
	denied func([]byte) error
	sent   atomic.Bool
}

func (t *sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "http" || r.URL.Host != "127.0.0.1:9997" || r.URL.User != nil || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || r.URL.RawPath != "" {
		return nil, errRefused
	}
	write := r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/login"
	read := r.Method == http.MethodGet && r.URL.Path == "/api/v1/controller-info"
	if !write && !read || write && (t.retain == nil || !t.sent.CompareAndSwap(false, true)) {
		return nil, errRefused
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, errRefused
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return nil, errRefused
	}
	if write && t.retain(response.StatusCode, raw) != nil {
		return nil, errRefused
	}
	if read && response.StatusCode == http.StatusUnauthorized && t.denied != nil {
		if t.denied(raw) != nil {
			return nil, errRefused
		}
		response.Body = io.NopCloser(bytes.NewReader(raw))
		return response, nil
	}
	if response.StatusCode != http.StatusOK || unambiguous(raw) != nil {
		return nil, errRefused
	}
	if write {
		var body struct {
			Token *string `json:"token"`
		}
		if strictObject(raw, &body) != nil || body.Token == nil || !validToken(*body.Token) {
			return nil, errRefused
		}
	} else {
		var body map[string]json.RawMessage
		var controller, version *string
		if json.Unmarshal(raw, &body) != nil || json.Unmarshal(body["controller_id"], &controller) != nil || controller == nil || !uuidPattern.MatchString(*controller) || json.Unmarshal(body["version"], &version) != nil || version == nil || *version != "0.2.1" {
			return nil, errRefused
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

type sessionClient struct {
	api   *garm.GarmAPI
	close func()
	wire  *sessionTransport
}

// Binding after Dial and before returning the connection prevents HTTP from
// sending credential bytes to a replacement listener between observations.
func dialSession(ctx context.Context, network, address string, verify func(context.Context) error) (net.Conn, error) {
	if verify == nil || ctx.Err() != nil {
		return nil, errRefused
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, errRefused
	}
	if verify(ctx) != nil || ctx.Err() != nil {
		conn.Close()
		return nil, errRefused
	}
	return conn, nil
}

func newBoundSessionClient(retain func(int, []byte) error, fixture http.RoundTripper, verify func(context.Context) error) sessionClient {
	base := fixture
	close := func() {}
	if base == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DisableKeepAlives = true
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialSession(ctx, network, address, verify)
		}
		base, close = transport, transport.CloseIdleConnections
	}
	wire := &sessionTransport{base: base, retain: retain}
	client := &http.Client{
		Transport: wire,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRefused
		},
	}
	rt := httptransport.NewWithClient("127.0.0.1:9997", "/api/v1", []string{"http"}, client)
	rt.Debug = false
	rt.Consumers = map[string]runtime.Consumer{"application/json": runtime.ConsumerFunc(func(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) })}
	return sessionClient{api: garm.New(rt, nil), close: close, wire: wire}
}

func (c sessionClient) rejected(ctx context.Context, token string) error {
	return c.rejection(ctx, token, nil)
}

func (c sessionClient) rejection(ctx context.Context, token string, retain func([]byte) error) error {
	if !validToken(token) || c.wire == nil || c.wire.denied != nil {
		return errRefused
	}
	observed := false
	c.wire.denied = func(raw []byte) error {
		var body struct {
			Error   *string `json:"error"`
			Details *string `json:"details"`
		}
		if strictObject(raw, &body) != nil || body.Error == nil || *body.Error != "Authentication failed" || body.Details == nil || *body.Details != "" {
			return errRefused
		}
		if retain != nil && retain(raw) != nil {
			return errRefused
		}
		observed = true
		return nil
	}
	defer func() { c.wire.denied = nil }()
	_, err := c.api.ControllerInfo.ControllerInfo(controller_info.NewControllerInfoParamsWithContext(ctx).WithTimeout(15*time.Second), httptransport.BearerToken(token))
	if err == nil || !observed {
		return errRefused
	}
	return nil
}

func (c sessionClient) login(ctx context.Context, username, password string) (string, error) {
	if username == "" || password == "" {
		return "", errRefused
	}
	p := login.NewLoginParamsWithContext(ctx).WithTimeout(15 * time.Second)
	p.Body = params.PasswordLoginParams{Username: username, Password: password}
	result, err := c.api.Login.Login(p, nil)
	if err != nil || result == nil || !validToken(result.Payload.Token) {
		return "", errRefused
	}
	return result.Payload.Token, nil
}

func (c sessionClient) controller(ctx context.Context, token, controller string) error {
	if !validToken(token) || !uuidPattern.MatchString(controller) {
		return errRefused
	}
	result, err := c.api.ControllerInfo.ControllerInfo(controller_info.NewControllerInfoParamsWithContext(ctx).WithTimeout(15*time.Second), httptransport.BearerToken(token))
	if err != nil || result == nil || result.Payload.ControllerID.String() != controller || result.Payload.Version != "0.2.1" {
		return errRefused
	}
	return nil
}

// JWT claims are a projection of the retained loopback response, not signature
// verification. A successful authenticated controller GET is also required.
func sessionExpiry(token string, started, ended time.Time) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 64*1024 || !validToken(token) || ended.Before(started) {
		return 0, errRefused
	}
	header, e := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if e != nil {
		return 0, errRefused
	}
	var h struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if strictObject(header, &h) != nil || h.Algorithm != "HS256" || h.Type != "JWT" {
		return 0, errRefused
	}
	signature, e := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if e != nil || len(signature) != 32 {
		return 0, errRefused
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if e != nil {
		return 0, errRefused
	}
	var claims struct {
		User       *string `json:"user"`
		ID         *string `json:"token_id"`
		FullName   *string `json:"full_name"`
		Admin      *bool   `json:"is_admin"`
		Metrics    *bool   `json:"read_metrics"`
		Generation *uint   `json:"generation"`
		Expires    *int64  `json:"exp"`
		Issuer     *string `json:"iss"`
	}
	if strictObject(raw, &claims) != nil || claims.User == nil || *claims.User == "" || claims.ID == nil || len(*claims.ID) != 16 || claims.FullName == nil || claims.Admin == nil || !*claims.Admin || claims.Metrics == nil || *claims.Metrics || claims.Generation == nil || claims.Expires == nil || claims.Issuer == nil || *claims.Issuer != "garm" {
		return 0, errRefused
	}
	// GARM v0.2.1 serializes exp in whole seconds. The mint instant must fall
	// inside the request interval; this does not establish the pre-send TTL.
	if *claims.Expires < started.Unix()+86400 || *claims.Expires > ended.Unix()+86400 || *claims.Expires <= ended.Unix() {
		return 0, errRefused
	}
	return *claims.Expires, nil
}
