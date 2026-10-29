package lifecycle

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/cloudbase/garm/auth"
	jwt "github.com/golang-jwt/jwt/v5"
)

type sessionProbe struct {
	RequestSHA string `json:"request_sha256"`
	User       string `json:"user_id"`
	Generation int64  `json:"generation"`
	TokenSHA   string `json:"token_sha256"`
	Expires    int64  `json:"expires_at"`
}

type sessionDomainProof struct {
	ProbeSHA string `json:"probe_sha256"`
	Old      string `json:"old_invocation"`
	New      string `json:"new_invocation"`
	Rejected int    `json:"old_signature_status"`
	Accepted int    `json:"new_signature_status"`
}

// These probes are locally signed administrative credentials used only by the
// fixed controller-info GET. They are not historical tokens recovered from a
// lost response, and their acceptance does not prove a historical request ran.
type sessionDomain struct {
	store      *serviceFiles
	plan       sessionPlan
	requestSHA string
	user       sessionDatabase
	now        func() time.Time
	fixture    http.RoundTripper
	services   sessionWindowServices
}

func (d sessionDomain) target(ctx context.Context, expected gatewayFacts, listener sessionSocket) error {
	if d.services == nil || ctx.Err() != nil {
		return errRefused
	}
	boot, _, err := d.services.identity(ctx)
	if err != nil || boot != d.plan.Boot {
		return errRefused
	}
	current, err := d.services.controller(ctx, nil)
	if err != nil || !equalSessionController(current, expected) {
		return errRefused
	}
	actual, err := d.services.listener(ctx, current.Main)
	if err != nil || actual != listener {
		return errRefused
	}
	return nil
}

func sessionSigningKey(raw []byte) (string, error) {
	var config struct {
		JWT struct {
			Secret string `toml:"secret"`
		} `toml:"jwt_auth"`
	}
	if len(raw) > 64*1024 {
		return "", errRefused
	}
	if _, err := toml.Decode(string(raw), &config); err != nil || config.JWT.Secret == "" {
		return "", errRefused
	}
	return config.JWT.Secret, nil
}

func (d sessionDomain) read(before []byte) (sessionProbe, string, error) {
	a := sessionAcquirer{files: d.store.files}
	var p sessionProbe
	if _, err := a.read(d.store, "old-domain.probe.json", &p); err != nil || p.RequestSHA != d.requestSHA || p.User != d.user.User || p.Generation != d.user.Generation || p.Expires <= d.now().Unix() {
		return p, "", errRefused
	}
	raw, err := a.read(d.store, "old-domain.probe", nil)
	if err != nil || p.TokenSHA != bytesSHA(raw) || !bytes.HasSuffix(raw, []byte("\n")) {
		return p, "", errRefused
	}
	token := strings.TrimSuffix(string(raw), "\n")
	key, err := sessionSigningKey(before)
	if err != nil {
		return p, "", errRefused
	}
	claims := &auth.JWTClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return []byte(key), nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("garm"), jwt.WithExpirationRequired(), jwt.WithTimeFunc(d.now))
	if err != nil || parsed == nil || !parsed.Valid || claims.ExpiresAt == nil || claims.ExpiresAt.Unix() != p.Expires || claims.UserID != d.user.User || uint64(claims.Generation) != uint64(d.user.Generation) || !claims.IsAdmin || claims.ReadMetrics || claims.TokenID != "rotation-probe01" {
		return p, "", errRefused
	}
	return p, token, nil
}

func (d sessionDomain) prepare(ctx context.Context, before []byte) error {
	if d.store == nil || d.now == nil || d.services == nil || bytesSHA(before) != d.plan.ConfigSHA || !digestPattern.MatchString(d.requestSHA) || !uuidPattern.MatchString(d.user.User) || d.user.Generation < 0 || d.user.Controller != d.plan.Controller {
		return errRefused
	}
	a := sessionAcquirer{files: d.store.files}
	if !a.absent(d.store, "old-domain.probe.json") {
		_, _, err := d.read(before)
		return err
	}
	// An incomplete probe publication is not permission to mint a second one.
	if !a.absent(d.store, "old-domain.probe") || !a.absent(d.store, "controller.stop.intent.json") {
		return errRefused
	}
	key, err := sessionSigningKey(before)
	if err != nil {
		return errRefused
	}
	expires := d.now().Add(24 * time.Hour).Unix()
	claims := auth.JWTClaims{UserID: d.user.User, TokenID: "rotation-probe01", IsAdmin: true, Generation: uint(d.user.Generation), RegisteredClaims: jwt.RegisteredClaims{Issuer: "garm", ExpiresAt: jwt.NewNumericDate(time.Unix(expires, 0))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
	if err != nil {
		return errRefused
	}
	server, err := d.services.controller(ctx, nil)
	if err != nil || !sessionControllerRunning(server) || server.Invocation != d.plan.Invocation || server.Main.ExecutableSHA != d.plan.BinarySHA {
		return errRefused
	}
	listener, err := d.services.listener(ctx, server.Main)
	if err != nil || !listener.valid() {
		return errRefused
	}
	client := newBoundSessionClient(nil, d.fixture, func(c context.Context) error { return d.target(c, server, listener) })
	defer client.close()
	if client.controller(ctx, token, d.plan.Controller) != nil || d.target(ctx, server, listener) != nil {
		return errRefused
	}
	raw := []byte(token + "\n")
	i := sessionInstaller{store: d.store}
	if i.raw("old-domain.probe", raw) != nil || d.store.create("old-domain.probe.json", sessionProbe{d.requestSHA, d.user.User, d.user.Generation, bytesSHA(raw), expires}) != nil {
		return errRefused
	}
	return nil
}

func (d sessionDomain) verify(ctx context.Context, before, after []byte, current gatewayFacts) (string, error) {
	if d.services == nil || bytesSHA(before) != d.plan.ConfigSHA || !validSessionConfiguration(before, after) || current.Invocation == d.plan.Invocation || !sessionControllerRunning(current) {
		return "", errRefused
	}
	p, token, err := d.read(before)
	if err != nil {
		return "", errRefused
	}
	key, err := sessionSigningKey(after)
	if err != nil {
		return "", errRefused
	}
	// Re-sign the exact old claims. Success with the new key and 401 with the
	// old key distinguish key rotation from a disabled/deleted probe account.
	claims := &auth.JWTClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return "", errRefused
	}
	currentToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
	if err != nil {
		return "", errRefused
	}
	listener, err := d.services.listener(ctx, current.Main)
	if err != nil || !listener.valid() {
		return "", errRefused
	}
	client := newBoundSessionClient(nil, d.fixture, func(c context.Context) error { return d.target(c, current, listener) })
	defer client.close()
	if client.controller(ctx, currentToken, d.plan.Controller) != nil || client.rejected(ctx, token) != nil || d.now().Unix() >= p.Expires || d.target(ctx, current, listener) != nil {
		return "", errRefused
	}
	proof := sessionDomainProof{bytesSHA(d.store.raw["old-domain.probe.json"]), d.plan.Invocation, current.Invocation, 401, 200}
	a := sessionAcquirer{files: d.store.files}
	if a.absent(d.store, "old-domain.rejected.json") {
		if d.store.create("old-domain.rejected.json", proof) != nil {
			return "", errRefused
		}
	}
	var saved sessionDomainProof
	raw, err := a.read(d.store, "old-domain.rejected.json", &saved)
	if err != nil || saved != proof || !d.store.unchanged() {
		return "", errRefused
	}
	return bytesSHA(raw), nil
}
