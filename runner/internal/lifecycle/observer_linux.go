package lifecycle

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strconv"
	"time"

	"github.com/cloudbase/garm-provider-common/util"
	"github.com/golang-jwt/jwt/v5"
)

// Source identities are supplied by the trusted request, never inferred from a
// token's prefix or imported from a historical acquisition's unknown result.
type observerPlan struct {
	Version         int    `json:"version"`
	Operation       string `json:"operation_id"`
	Controller      string `json:"controller_id"`
	SessionProducer string `json:"garm_session_sha256"`
	SessionRequest  string `json:"garm_request_sha256"`
	Credential      int64  `json:"credential_id"`
	CredentialName  string `json:"credential_name"`
	Endpoint        string `json:"endpoint_name"`
	Updated         string `json:"credential_updated_at"`
	PayloadSHA      string `json:"payload_sha256"`
	App             int64  `json:"app_id"`
	Installation    int64  `json:"installation_id"`
	Account         int64  `json:"account_id"`
	Fingerprint     string `json:"public_key_sha256"`
	RequiredUntil   int64  `json:"required_until"`
}

func decodeObserverPlan(raw []byte) (observerPlan, error) {
	var p observerPlan
	if len(raw) > 64*1024 || strictObject(raw, &p) != nil || p.Version != 1 || !uuidPattern.MatchString(p.Operation) || !uuidPattern.MatchString(p.Controller) || !digestPattern.MatchString(p.SessionProducer) || !digestPattern.MatchString(p.SessionRequest) || p.Credential <= 0 || p.Credential > 4294967295 || p.CredentialName == "" || len(p.CredentialName) > 64 || p.Endpoint == "" || len(p.Endpoint) > 64 || p.Updated == "" || len(p.Updated) > 128 || !digestPattern.MatchString(p.PayloadSHA) || p.App <= 0 || p.Installation <= 0 || p.Account <= 0 || !digestPattern.MatchString(p.Fingerprint) || p.RequiredUntil <= 0 {
		return observerPlan{}, errRefused
	}
	return p, nil
}

type observerJWT struct {
	Token   string `json:"token"`
	Issued  int64  `json:"issued_at"`
	Expires int64  `json:"expires_at"`
}

func observerKey(p observerPlan, payload, passphrase []byte) (*rsa.PrivateKey, error) {
	if bytesSHA(payload) != p.PayloadSHA {
		return nil, errRefused
	}
	plain, err := util.Unseal(payload, passphrase)
	if err != nil {
		return nil, errRefused
	}
	defer clear(plain)
	var app struct {
		App          int64  `json:"app_id"`
		Installation int64  `json:"installation_id"`
		Key          []byte `json:"private_key_bytes"`
	}
	if strictObject(plain, &app) != nil {
		clear(app.Key)
		return nil, errRefused
	}
	defer clear(app.Key)
	if app.App != p.App || app.Installation != p.Installation {
		return nil, errRefused
	}
	block, rest := pem.Decode(app.Key)
	if block == nil || block.Type != "RSA PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errRefused
	}
	defer clear(block.Bytes)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil || key.Validate() != nil || key.N.BitLen() < 2048 {
		return nil, errRefused
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil || bytesSHA(public) != p.Fingerprint {
		return nil, errRefused
	}
	return key, nil
}

func signObserver(p observerPlan, key *rsa.PrivateKey, started int64) (observerJWT, error) {
	r := observerJWT{Issued: started - 60, Expires: started + 540}
	var err error
	r.Token, err = jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: strconv.FormatInt(p.App, 10), IssuedAt: jwt.NewNumericDate(time.Unix(r.Issued, 0)), ExpiresAt: jwt.NewNumericDate(time.Unix(r.Expires, 0))}).SignedString(key)
	if err != nil {
		return observerJWT{}, errRefused
	}
	return r, nil
}

func verifyObserverJWT(p observerPlan, key *rsa.PublicKey, r observerJWT, started int64) error {
	if r.Issued != started-60 || r.Expires != started+540 {
		return errRefused
	}
	claims := &jwt.RegisteredClaims{}
	t, err := jwt.ParseWithClaims(r.Token, claims, func(*jwt.Token) (any, error) { return key, nil }, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(strconv.FormatInt(p.App, 10)), jwt.WithExpirationRequired(), jwt.WithTimeFunc(func() time.Time { return time.Unix(started, 0) }), jwt.WithIssuedAt())
	if err != nil || !t.Valid || claims.IssuedAt == nil || claims.ExpiresAt == nil || claims.IssuedAt.Unix() != r.Issued || claims.ExpiresAt.Unix() != r.Expires {
		return errRefused
	}
	return nil
}

func observerPermissions(audit bool) map[string]string {
	p := map[string]string{"metadata": "read"}
	if !audit {
		p["actions"] = "read"
		p["administration"] = "read"
	}
	return p
}

// Unknown permission names and null/missing values are rejected on the raw
// response, before the SDK could silently discard a newly added permission.
func exactObserverPermissions(raw json.RawMessage, audit bool) bool {
	var actual map[string]string
	if unambiguous(raw) != nil || json.Unmarshal(raw, &actual) != nil {
		return false
	}
	want := observerPermissions(audit)
	if len(actual) != len(want) {
		return false
	}
	for k, v := range want {
		if actual[k] != v {
			return false
		}
	}
	return true
}
