package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type connectionRetirement struct {
	Request  string `json:"request_sha256"`
	Producer string `json:"producer_sha256"`
	Started  int64  `json:"started_at"`
}

type connectionCredentialState struct {
	Audit        string `json:"audit_installation"`
	Admin        string `json:"admin_installation"`
	Registration string `json:"registration"`
	Actions      string `json:"actions_admin"`
}

func (e connectionExecutor) retire(ctx context.Context, raw []byte) (connectionCredentialState, error) {
	p, err := decodeConnectionPlan(raw)
	if err != nil || e.now == nil || !digestPattern.MatchString(e.producer) {
		return connectionCredentialState{}, errRefused
	}
	s, err := openServiceFiles(e.files, e.parent, "connection", "session.lock", false)
	if err != nil {
		return connectionCredentialState{}, errRefused
	}
	defer s.close()
	if exists, err := s.existing(); err != nil || !exists {
		return connectionCredentialState{}, errRefused
	}
	reader := sessionAcquirer{files: e.files}
	original, err := reader.read(s, "request.json", nil)
	if err != nil || !bytes.Equal(original, raw) {
		return connectionCredentialState{}, errRefused
	}
	var origin connectionIntent
	if _, err := reader.read(s, "connection.intent.json", &origin); err != nil || origin.Producer != e.producer || origin.Request != bytesSHA(raw) || origin.Started <= 0 || e.now().Unix() < origin.Started {
		return connectionCredentialState{}, errRefused
	}
	intent := connectionRetirement{bytesSHA(raw), e.producer, e.now().Unix()}
	if reader.absent(s, "retirement.intent.json") {
		if s.create("retirement.intent.json", intent) != nil {
			return connectionCredentialState{}, errRefused
		}
	}
	var saved connectionRetirement
	if _, err := reader.read(s, "retirement.intent.json", &saved); err != nil || saved.Request != intent.Request || saved.Producer != intent.Producer || saved.Started < origin.Started || e.now().Unix() < saved.Started {
		return connectionCredentialState{}, errRefused
	}
	basic := func(c context.Context) error {
		if c.Err() != nil {
			return errRefused
		}
		return connectionClock(s, e.now().Unix())
	}
	api := connectionAPI{store: s, plan: p, producer: e.producer, now: e.now, fixture: e.fixture, check: basic}
	defer api.close()
	base := e.fixture
	if base == nil {
		transport := observerBase()
		api.transports = append(api.transports, transport)
		base = transport
	}
	installation := observerAPI{store: s, plan: p.Source, producer: e.producer, base: base, now: e.now, check: basic}
	state := connectionCredentialState{Audit: "not_attempted", Admin: "not_attempted", Registration: "not_attempted", Actions: "not_attempted"}
	for _, name := range []string{"audit", "admin"} {
		if reader.absent(s, name+"-mint.intent.json") {
			continue
		}
		r, err := api.response(name + "-mint")
		if err != nil {
			return state, errRefused
		}
		t, _ := connectionToken(r.Body)
		if !validToken(t.token) {
			return state, errRefused
		}
		status, err := installation.retire(ctx, name, t)
		if err != nil {
			return state, errRefused
		}
		if name == "audit" {
			state.Audit = status
		} else {
			state.Admin = status
		}
	}
	if !reader.absent(s, "registration.intent.json") {
		r, err := api.response("registration")
		if err != nil {
			return state, errRefused
		}
		var wire struct {
			Token   *string    `json:"token"`
			Expires *time.Time `json:"expires_at"`
		}
		if r.Status != 201 || unambiguous(r.Body) != nil || json.Unmarshal(r.Body, &wire) != nil || wire.Token == nil || !validToken(*wire.Token) || wire.Expires == nil || wire.Expires.Unix() <= r.Observed || wire.Expires.Unix() > r.Observed+3600 {
			return state, errRefused
		}
		state.Registration = "protected_until_documented_expiry"
		if e.now().Unix() >= wire.Expires.Unix() {
			state.Registration = "documented_expiry_elapsed_rejection_unverified"
		}
	}
	if !reader.absent(s, "remote-auth.intent.json") {
		r, err := api.response("remote-auth")
		if err != nil || r.Status != 200 {
			return state, errRefused
		}
		actions, err := parseConnectionActions(r.Body, r.Observed, p.Source.RequiredUntil)
		if err != nil {
			return state, errRefused
		}
		state.Actions = "protected_until_original_expiry"
		if e.now().Unix() >= actions.Expires {
			if reader.absent(s, "actions.expired.json") {
				step, err := api.nextRead("actions-expiry")
				if err != nil {
					return state, err
				}
				response, err := api.actionsRead(ctx, actions, step)
				if err != nil || response.Status != http.StatusUnauthorized || response.Observed < actions.Expires {
					return state, errRefused
				}
				if s.create("actions.expired.json", struct {
					Step     string `json:"step"`
					Response string `json:"response_sha256"`
					Observed int64  `json:"observed_at"`
				}{step, bytesSHA(s.raw[step+".response.json"]), response.Observed}) != nil {
					return state, errRefused
				}
			}
			var fact struct {
				Step     string `json:"step"`
				Response string `json:"response_sha256"`
				Observed int64  `json:"observed_at"`
			}
			if _, err := reader.read(s, "actions.expired.json", &fact); err != nil || fact.Observed < actions.Expires || fact.Observed > e.now().Unix() {
				return state, errRefused
			}
			response, err := api.response(fact.Step)
			if err != nil || response.Status != 401 || bytesSHA(s.raw[fact.Step+".response.json"]) != fact.Response || response.Observed != fact.Observed {
				return state, errRefused
			}
			state.Actions = "expiry_rejection_observed"
		}
	}
	if state.Registration == "protected_until_documented_expiry" || state.Actions == "protected_until_original_expiry" {
		return state, nil
	}
	if reader.absent(s, "credentials.retired.json") {
		if s.create("credentials.retired.json", state) != nil {
			return state, errRefused
		}
	}
	var retained connectionCredentialState
	if _, err := reader.read(s, "credentials.retired.json", &retained); err != nil || retained != state || basic(ctx) != nil {
		return state, errRefused
	}
	return state, nil
}
