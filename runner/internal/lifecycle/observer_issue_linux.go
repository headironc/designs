package lifecycle

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type observerOrigin struct {
	Session     string `json:"session_complete_sha256"`
	Database    string `json:"database_sha256"`
	Config      string `json:"configuration_sha256"`
	Invocation  string `json:"controller_invocation"`
	GARMExpires int64  `json:"garm_expires_at"`
}

type observerIntent struct {
	Version  int            `json:"version"`
	Request  string         `json:"request_sha256"`
	Producer string         `json:"producer_sha256"`
	Started  int64          `json:"started_at"`
	Origin   observerOrigin `json:"origin"`
}

type observerSeal struct {
	Version  int    `json:"version"`
	Request  string `json:"request_sha256"`
	Producer string `json:"producer_sha256"`
	Records  string `json:"records_sha256"`
	Token    string `json:"token_sha256"`
	Expires  int64  `json:"expires_at"`
	Until    int64  `json:"required_until"`
}

type observerAdmission struct {
	key    *rsa.PrivateKey
	origin observerOrigin
	check  func(context.Context) error
}

type observerIssuer struct {
	files            protectedFiles
	parent, producer string
	now              func() time.Time
	fixture          http.RoundTripper
	admit            func(context.Context, observerPlan, *serviceFiles, func(observerAdmission) error) error
}

func observerBase() *http.Transport {
	return &http.Transport{Proxy: nil, DisableKeepAlives: true, ForceAttemptHTTP2: false, ResponseHeaderTimeout: 20 * time.Second, TLSHandshakeTimeout: 10 * time.Second, MaxResponseHeaderBytes: 32 * 1024}
}

func observerWriteRaw(s *serviceFiles, name string, raw []byte) error {
	o := operation{files: s.files, parent: s.parent, dir: s.dir, path: s.path}
	if !s.unchanged() || o.create(name, raw) != nil || s.files.syncFile(s.dir) != nil || s.track(name, raw) != nil {
		return errRefused
	}
	return nil
}

func observerClock(s *serviceFiles, now int64) error {
	if now <= 0 || !s.unchanged() {
		return errRefused
	}
	for name, raw := range s.raw {
		if strings.HasSuffix(name, ".response.json") && name != "sign.response.json" {
			var r observerResponse
			if strictObject(raw, &r) != nil || r.Observed > now {
				return errRefused
			}
		}
		if strings.HasSuffix(name, ".intent.json") && name != "sign.intent.json" && name != "retirement.intent.json" {
			var i observerCall
			if strictObject(raw, &i) != nil || i.Started > now {
				return errRefused
			}
		}
	}
	return nil
}

func (a observerIssuer) execute(ctx context.Context, raw []byte) (sessionOutcome, error) {
	p, err := decodeObserverPlan(raw)
	if err != nil || a.now == nil || a.admit == nil || !digestPattern.MatchString(a.producer) {
		return sessionOutcome{}, errRefused
	}
	s, err := openServiceFiles(a.files, a.parent, "github-observer", "session.lock", false)
	if err != nil {
		return sessionOutcome{}, errRefused
	}
	defer s.close()
	exists, err := s.existing()
	if err != nil {
		return sessionOutcome{}, errRefused
	}
	reader := sessionAcquirer{files: a.files}
	if exists {
		original, err := reader.read(s, "request.json", nil)
		if err != nil || !bytes.Equal(original, raw) {
			return sessionOutcome{}, errRefused
		}
		if !reader.absent(s, "retirement.intent.json") || !reader.absent(s, "aborted.json") {
			return sessionOutcome{}, errRefused
		}
		if !reader.absent(s, "complete.json") {
			seal, _, err := verifyObserverOriginal(s, a.producer, a.now().Unix(), false)
			if err != nil {
				return sessionOutcome{}, errRefused
			}
			return sessionOutcome{State: "original", Expires: seal.Expires}, nil
		}
	}
	base := a.fixture
	if base == nil {
		transport := observerBase()
		defer transport.CloseIdleConnections()
		base = transport
	}
	var out sessionOutcome
	called := false
	err = a.admit(ctx, p, s, func(ad observerAdmission) error {
		if called || ad.key == nil || ad.check == nil {
			return errRefused
		}
		called = true
		var err error
		out, err = a.issue(ctx, p, raw, s, exists, base, ad)
		return err
	})
	if err != nil || !called {
		return sessionOutcome{State: "unknown"}, errRefused
	}
	return out, nil
}

func (a observerIssuer) issue(ctx context.Context, p observerPlan, raw []byte, s *serviceFiles, exists bool, base http.RoundTripper, ad observerAdmission) (sessionOutcome, error) {
	reader := sessionAcquirer{files: a.files}
	if !digestPattern.MatchString(ad.origin.Session) || !digestPattern.MatchString(ad.origin.Database) || !digestPattern.MatchString(ad.origin.Config) || !pauseHex.MatchString(ad.origin.Invocation) || ad.origin.GARMExpires < p.RequiredUntil {
		return sessionOutcome{}, errRefused
	}
	basic := func(c context.Context) error {
		if c.Err() != nil || observerClock(s, a.now().Unix()) != nil {
			return errRefused
		}
		return nil
	}
	check := func(c context.Context) error {
		if basic(c) != nil || a.now().Unix() >= p.RequiredUntil || a.now().Unix() >= ad.origin.GARMExpires || ad.check(c) != nil {
			return errRefused
		}
		return nil
	}
	if check(ctx) != nil {
		return sessionOutcome{}, errRefused
	}
	if !exists {
		if p.RequiredUntil > a.now().Unix()+3600 || s.reserve() != nil || observerWriteRaw(s, "request.json", raw) != nil {
			return sessionOutcome{}, errRefused
		}
		i := observerIntent{1, bytesSHA(raw), a.producer, a.now().Unix(), ad.origin}
		if s.create("sign.intent.json", i) != nil || check(ctx) != nil {
			return sessionOutcome{}, errRefused
		}
		// Once sign.intent exists, no recovery branch signs a replacement JWT.
		token, err := signObserver(p, ad.key, i.Started)
		if err != nil || s.create("sign.response.json", token) != nil {
			return sessionOutcome{}, errRefused
		}
	}
	var intent observerIntent
	if _, err := reader.read(s, "sign.intent.json", &intent); err != nil || intent.Version != 1 || intent.Request != bytesSHA(raw) || intent.Producer != a.producer || intent.Origin != ad.origin || intent.Started <= 0 || a.now().Unix() < intent.Started {
		return sessionOutcome{}, errRefused
	}
	var signed observerJWT
	if _, err := reader.read(s, "sign.response.json", &signed); err != nil || verifyObserverJWT(p, &ad.key.PublicKey, signed, intent.Started) != nil {
		return sessionOutcome{}, errRefused
	}
	issueCheck := func(c context.Context) error {
		if check(c) != nil || a.now().Unix() >= signed.Expires {
			return errRefused
		}
		return nil
	}
	api := observerAPI{store: s, plan: p, producer: a.producer, base: base, now: a.now, check: issueCheck}
	retireAPI := api
	retireAPI.check = basic
	abort := func(step string, t observerToken) error {
		if reader.absent(s, "aborted.json") && s.create("aborted.json", struct {
			Step    string `json:"step"`
			Request string `json:"request_sha256"`
		}{step, bytesSHA(raw)}) != nil {
			return errRefused
		}
		if validToken(t.token) {
			_, _ = retireAPI.retire(ctx, step, t)
		}
		return errRefused
	}
	before, err := api.installation(ctx, "installation-before", signed.Token)
	if err != nil {
		return sessionOutcome{}, errRefused
	}
	if p.RequiredUntil > a.now().Unix()+3600 || issueCheck(ctx) != nil {
		return sessionOutcome{}, errRefused
	}
	if api.mint(ctx, "audit-mint", signed.Token, true) != nil {
		return sessionOutcome{}, errRefused
	}
	audit, err := api.minted("audit-mint", true)
	if err != nil {
		return sessionOutcome{}, abort("audit", audit)
	}
	if api.repositories(ctx, "audit-repositories", audit.token) != nil {
		return sessionOutcome{}, abort("audit", audit)
	}
	after, err := api.installation(ctx, "installation-after", signed.Token)
	if err != nil || before != after {
		return sessionOutcome{}, abort("audit", audit)
	}
	state, err := retireAPI.retire(ctx, "audit", audit)
	if err != nil || state != "revocation_rejection_observed" {
		return sessionOutcome{}, errRefused
	}
	if issueCheck(ctx) != nil || p.RequiredUntil > a.now().Unix()+3600 {
		return sessionOutcome{}, errRefused
	}
	if api.mint(ctx, "observer-mint", signed.Token, false) != nil {
		return sessionOutcome{}, errRefused
	}
	token, err := api.minted("observer-mint", false)
	if err != nil || token.expires < p.RequiredUntil || a.now().Unix() >= token.expires {
		return sessionOutcome{}, abort("observer", token)
	}
	if api.repositories(ctx, "observer-repositories", token.token) != nil {
		return sessionOutcome{}, abort("observer", token)
	}
	if issueCheck(ctx) != nil {
		return sessionOutcome{}, abort("observer", token)
	}
	seal, err := observerVerified(s, p, a.producer)
	if err != nil {
		return sessionOutcome{}, errRefused
	}
	if reader.absent(s, "verified.json") {
		if s.create("verified.json", seal) != nil {
			return sessionOutcome{}, errRefused
		}
	}
	var saved observerSeal
	if _, err := reader.read(s, "verified.json", &saved); err != nil || saved != seal {
		return sessionOutcome{}, errRefused
	}
	tokenRaw := []byte(token.token + "\n")
	if reader.absent(s, "github-token") {
		if observerWriteRaw(s, "github-token", tokenRaw) != nil {
			return sessionOutcome{}, errRefused
		}
	}
	got, err := reader.read(s, "github-token", nil)
	if err != nil || !bytes.Equal(got, tokenRaw) || issueCheck(ctx) != nil || s.create("complete.json", seal) != nil {
		return sessionOutcome{}, errRefused
	}
	return sessionOutcome{State: "acquired", Expires: token.expires}, nil
}

var observerIssueSteps = []string{"installation-before", "audit-mint", "audit-repositories", "installation-after", "audit-delete", "audit-rejection", "observer-mint", "observer-repositories"}

// Reconstruct the original verified judgment from its protected facts. No
// consumer can accept a token solely because a copied seal names its hash.
func observerVerified(s *serviceFiles, p observerPlan, producer string) (observerSeal, error) {
	reader := sessionAcquirer{files: s.files}
	var sign observerIntent
	var signed observerJWT
	if _, err := reader.read(s, "sign.intent.json", &sign); err != nil || sign.Version != 1 || sign.Request != bytesSHA(s.raw["request.json"]) || sign.Producer != producer || sign.Started <= 0 || sign.Origin.GARMExpires < p.RequiredUntil || !digestPattern.MatchString(sign.Origin.Session) || !digestPattern.MatchString(sign.Origin.Database) || !digestPattern.MatchString(sign.Origin.Config) || !pauseHex.MatchString(sign.Origin.Invocation) {
		return observerSeal{}, errRefused
	}
	if _, err := reader.read(s, "sign.response.json", &signed); err != nil || signed.Issued != sign.Started-60 || signed.Expires != sign.Started+540 || !validToken(signed.Token) {
		return observerSeal{}, errRefused
	}
	sums := map[string]string{"request": bytesSHA(s.raw["request.json"]), "sign-intent": bytesSHA(s.raw["sign.intent.json"]), "sign-response": bytesSHA(s.raw["sign.response.json"])}
	last := sign.Started
	for _, step := range observerIssueSteps {
		var i observerCall
		var r observerResponse
		if _, err := reader.read(s, step+".intent.json", &i); err != nil {
			return observerSeal{}, errRefused
		}
		if _, err := reader.read(s, step+".response.json", &r); err != nil || i.Version != 1 || i.Request != sign.Request || i.Producer != producer || i.Step != step || i.Started < last || r.Intent != bytesSHA(s.raw[step+".intent.json"]) || r.Observed < i.Started || r.Observed >= signed.Expires || r.Observed >= sign.Origin.GARMExpires {
			return observerSeal{}, errRefused
		}
		last = r.Observed
		sums[step+"-intent"] = bytesSHA(s.raw[step+".intent.json"])
		sums[step+"-response"] = bytesSHA(s.raw[step+".response.json"])
	}
	api := observerAPI{store: s, plan: p, producer: producer}
	audit, err := api.minted("audit-mint", true)
	if err != nil {
		return observerSeal{}, errRefused
	}
	token, err := api.minted("observer-mint", false)
	if err != nil || token.expires < p.RequiredUntil || token.expires <= last {
		return observerSeal{}, errRefused
	}
	// Reuse the actual SDK and fixed transport in response replay mode. The
	// nil base cannot be reached because every required response was loaded.
	api.producer = producer
	api.now = func() time.Time { return time.Unix(last, 0) }
	if api.mint(context.Background(), "audit-mint", signed.Token, true) != nil || api.mint(context.Background(), "observer-mint", signed.Token, false) != nil {
		return observerSeal{}, errRefused
	}
	before, err := api.installation(context.Background(), "installation-before", signed.Token)
	if err != nil {
		return observerSeal{}, errRefused
	}
	after, err := api.installation(context.Background(), "installation-after", signed.Token)
	if err != nil || before != after {
		return observerSeal{}, errRefused
	}
	if api.repositories(context.Background(), "audit-repositories", audit.token) != nil || api.repositories(context.Background(), "observer-repositories", token.token) != nil {
		return observerSeal{}, errRefused
	}
	state, err := api.retire(context.Background(), "audit", audit)
	if err != nil || state != "revocation_rejection_observed" {
		return observerSeal{}, errRefused
	}
	records, _ := json.Marshal(sums)
	return observerSeal{1, sign.Request, producer, bytesSHA(records), bytesSHA([]byte(token.token + "\n")), token.expires, p.RequiredUntil}, nil
}

func verifyObserverOriginal(s *serviceFiles, producer string, now int64, retiring bool) (observerSeal, observerPlan, error) {
	reader := sessionAcquirer{files: s.files}
	raw, err := reader.read(s, "request.json", nil)
	if err != nil {
		return observerSeal{}, observerPlan{}, errRefused
	}
	p, err := decodeObserverPlan(raw)
	if err != nil {
		return observerSeal{}, p, errRefused
	}
	expected, err := observerVerified(s, p, producer)
	if err != nil {
		return observerSeal{}, p, errRefused
	}
	var complete, verified observerSeal
	if _, err := reader.read(s, "complete.json", &complete); err != nil || complete != expected {
		return observerSeal{}, p, errRefused
	}
	if _, err := reader.read(s, "verified.json", &verified); err != nil || verified != complete || observerClock(s, now) != nil || !retiring && (now >= complete.Until || now >= complete.Expires || !reader.absent(s, "retirement.intent.json") || !reader.absent(s, "aborted.json")) {
		return observerSeal{}, p, errRefused
	}
	return complete, p, nil
}

// Keep paths fixed. An external raw file remains a distinct, unsealed input.
func loadGitHubToken(files protectedFiles, path, parent, producer string, now time.Time) (*sessionCredential, error) {
	if path != filepath.Join(parent, "github-observer", "github-token") {
		if producer != "" {
			return nil, errRefused
		}
		raw, err := files.read(path, 0600, 64*1024+1)
		token := strings.TrimSpace(string(raw))
		if err != nil || !validToken(token) {
			return nil, errRefused
		}
		return &sessionCredential{token: token}, nil
	}
	if !digestPattern.MatchString(producer) || files.parents(filepath.Join(parent, "session.lock")) != nil {
		return nil, errRefused
	}
	dir, err := openPath(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, errRefused
	}
	s := &serviceFiles{files: files, parent: dir, path: filepath.Join(parent, "github-observer"), lockPath: filepath.Join(parent, "session.lock"), raw: map[string][]byte{}, pinned: map[string]*os.File{}}
	accepted := false
	defer func() {
		if !accepted {
			s.close()
		}
	}()
	if err := observerSharedLease(s); err != nil {
		return nil, errRefused
	}
	if exists, err := s.existing(); err != nil || !exists {
		return nil, errRefused
	}
	seal, p, err := verifyObserverOriginal(s, producer, now.Unix(), false)
	if err != nil {
		return nil, errRefused
	}
	raw, err := (sessionAcquirer{files: files}).read(s, "github-token", nil)
	if err != nil || bytesSHA(raw) != seal.Token || !validToken(strings.TrimSuffix(string(raw), "\n")) {
		return nil, errRefused
	}
	accepted = true
	return &sessionCredential{token: strings.TrimSuffix(string(raw), "\n"), controller: p.Controller, store: s, expires: seal.Expires}, nil
}

func observerSharedLease(s *serviceFiles) error {
	var err error
	s.lock, err = openPath(s.lockPath, unix.O_RDONLY, 0)
	if err != nil || metadata(s.parent, s.files.uid, unix.S_IFDIR|0700) != nil || metadata(s.lock, s.files.uid, unix.S_IFREG|0600) != nil || unix.Flock(int(s.lock.Fd()), unix.LOCK_SH|unix.LOCK_NB) != nil || !s.unchanged() {
		return errRefused
	}
	return nil
}
