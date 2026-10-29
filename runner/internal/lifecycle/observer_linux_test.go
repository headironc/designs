package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudbase/garm-provider-common/util"
)

type observerFixture struct {
	issuer               observerIssuer
	plan                 observerPlan
	raw                  []byte
	clock                int64
	posts, deletes, gets int
	revoked              map[string]bool
	mutate               func(*http.Request, *http.Response) error
	key                  *rsa.PrivateKey
}

func observerFixtureFor(t *testing.T) *observerFixture {
	t.Helper()
	ga, request, _, _ := sessionAcquisitionFixture(t)
	if _, err := ga.execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	f := &observerFixture{clock: ga.now().Unix(), revoked: map[string]bool{}, key: key}
	f.plan = observerPlan{Version: 1, Operation: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Controller: "bbbbbbbb-0000-4000-8000-000000000002", SessionProducer: ga.producer, SessionRequest: bytesSHA(request), Credential: 1, CredentialName: "nucleiron", Endpoint: "public", Updated: "2026-10-29 00:00:00+00:00", PayloadSHA: strings.Repeat("e", 64), App: 42, Installation: 43, Account: 44, Fingerprint: bytesSHA(public), RequiredUntil: f.clock + 1800}
	f.raw, _ = json.Marshal(f.plan)
	f.issuer = observerIssuer{files: ga.files, parent: ga.parent, producer: strings.Repeat("c", 64), now: func() time.Time { return time.Unix(f.clock, 0) }, admit: func(ctx context.Context, p observerPlan, s *serviceFiles, consume func(observerAdmission) error) error {
		other, err := s.sibling("session")
		if err != nil {
			return err
		}
		credential, err := readManagedGARM(other, p.SessionProducer, time.Unix(f.clock, 0))
		if err != nil {
			other.close()
			return err
		}
		defer credential.close()
		// This independent open must conflict throughout source reading: dup
		// must not downgrade the issuer's original exclusive flock.
		if competing, err := openServiceFiles(ga.files, ga.parent, "github-observer", "session.lock", false); err == nil {
			competing.close()
			t.Fatal("exclusive lease downgraded")
		}
		return consume(observerAdmission{key: key, origin: observerOrigin{bytesSHA(other.raw["complete.json"]), strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 32), credential.expires}, check: func(context.Context) error { return nil }})
	}}
	f.issuer.fixture = sessionRoundTripFunc(func(r *http.Request) (*http.Response, error) { return f.respond(t, r) })
	return f
}

func (f *observerFixture) respond(t *testing.T, r *http.Request) (*http.Response, error) {
	t.Helper()
	status := 200
	body := ""
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.Method == "GET" && r.URL.Path == "/app/installations/43":
		f.gets++
		body = `{"id":43,"app_id":42,"account":{"id":44,"login":"headironc"},"repository_selection":"selected","permissions":{"metadata":"read","actions":"write","administration":"write"},"suspended_at":null}`
	case r.Method == "POST" && r.URL.Path == "/app/installations/43/access_tokens":
		f.posts++
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if json.Unmarshal(raw, &wire) != nil {
			t.Fatal("mint JSON")
		}
		name := "audit"
		permissions := `{"metadata":"read"}`
		if _, ok := wire["repository_ids"]; ok {
			if string(wire["repository_ids"]) != "[1432619931]" {
				t.Fatal("observer scope")
			}
			name = "observer"
			permissions = `{"metadata":"read","actions":"read","administration":"read"}`
		}
		if len(wire) != 1 && name == "audit" || len(wire) != 2 && name == "observer" || !exactObserverPermissions(wire["permissions"], name == "audit") {
			t.Fatal("mint permission drift")
		}
		status = 201
		body = fmt.Sprintf(`{"token":%q,"expires_at":%q,"permissions":%s,"repository_selection":"selected"}`, "fixture-"+name+strings.Repeat("x", 160), time.Unix(f.clock+3600, 0).UTC().Format(time.RFC3339), permissions)
	case r.Method == "DELETE" && r.URL.Path == "/installation/token":
		f.deletes++
		f.revoked[token] = true
		status = 204
	case r.Method == "GET" && r.URL.Path == "/installation/repositories":
		f.gets++
		if f.revoked[token] {
			status = 401
			body = `{"message":"Bad credentials"}`
		} else {
			body = `{"total_count":1,"repositories":[{"id":1432619931,"name":"nucleus","full_name":"headironc/nucleus","owner":{"id":44,"login":"headironc"}}]}`
		}
	default:
		t.Fatalf("unapproved request %s %s", r.Method, r.URL)
		return nil, errRefused
	}
	response := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}
	if f.mutate != nil {
		if err := f.mutate(r, response); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func TestObserverIssueConsumerAndOriginalRecovery(t *testing.T) {
	f := observerFixtureFor(t)
	out, err := f.issuer.execute(context.Background(), f.raw)
	if err != nil || out.State != "acquired" || f.posts != 2 || f.deletes != 1 {
		t.Fatalf("issue: %+v %v posts=%d deletes=%d gets=%d", out, err, f.posts, f.deletes, f.gets)
	}
	path := filepath.Join(f.issuer.parent, "github-observer", "github-token")
	c, err := loadGitHubToken(f.issuer.files, path, f.issuer.parent, f.issuer.producer, time.Unix(f.clock, 0))
	if err != nil {
		t.Fatal("consumer", err)
	}
	second, err := loadGitHubToken(f.issuer.files, path, f.issuer.parent, f.issuer.producer, time.Unix(f.clock, 0))
	if err != nil {
		t.Fatal("shared consumer", err)
	}
	if c.controller != f.plan.Controller || len(c.token) < 100 {
		t.Fatal("consumer binding or long token")
	}
	r := observerRetirer{files: f.issuer.files, parent: f.issuer.parent, issuer: f.issuer.producer, producer: f.issuer.producer, now: f.issuer.now, fixture: f.issuer.fixture}
	if _, err := r.execute(context.Background(), f.raw); err == nil {
		t.Fatal("retired under active reader")
	}
	c.close()
	second.close()
	gets := f.gets
	f.issuer.admit = func(context.Context, observerPlan, *serviceFiles, func(observerAdmission) error) error {
		t.Fatal("original invoked admission")
		return errRefused
	}
	out, err = f.issuer.execute(context.Background(), f.raw)
	if err != nil || out.State != "original" || f.gets != gets || f.posts != 2 {
		t.Fatal("original recovery contacted service", err)
	}
	out, err = r.execute(context.Background(), f.raw)
	if err != nil || out.State != "original_tokens_retired_consumer_copy_removed" || f.deletes != 2 {
		t.Fatalf("retire %+v %v deletes=%d", out, err, f.deletes)
	}
	gets = f.gets
	if _, err := r.execute(context.Background(), f.raw); err != nil || f.gets != gets || f.deletes != 2 {
		t.Fatal("retirement replay contacted remote", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("consumer copy survived")
	}
	if _, err := loadGitHubToken(f.issuer.files, path, f.issuer.parent, f.issuer.producer, time.Unix(f.clock, 0)); err == nil {
		t.Fatal("retired consumer accepted")
	}
}

func TestObserverUnknownMintAndDeleteNeverReplay(t *testing.T) {
	for _, method := range []string{"POST", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			f := observerFixtureFor(t)
			f.mutate = func(r *http.Request, _ *http.Response) error {
				if r.Method == method {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			if _, err := f.issuer.execute(context.Background(), f.raw); err == nil {
				t.Fatal("lost receipt accepted")
			}
			posts, deletes := f.posts, f.deletes
			f.mutate = nil
			if _, err := f.issuer.execute(context.Background(), f.raw); err == nil {
				t.Fatal("unknown outcome accepted")
			}
			if f.posts != posts || f.deletes != deletes {
				t.Fatal("external write replayed")
			}
		})
	}
}

func TestObserverRejectsScopePermissionAndInsufficientLifetime(t *testing.T) {
	for _, name := range []string{"extra_permission", "missing_permission", "extra_repository", "next_link", "suspended", "expiry_short", "too_long_request", "clock_rollback"} {
		t.Run(name, func(t *testing.T) {
			f := observerFixtureFor(t)
			if name == "too_long_request" {
				f.plan.RequiredUntil = f.clock + 3601
				f.raw, _ = json.Marshal(f.plan)
			}
			f.mutate = func(r *http.Request, response *http.Response) error {
				raw, _ := io.ReadAll(response.Body)
				response.Body.Close()
				body := string(raw)
				switch name {
				case "extra_permission":
					if r.Method == "POST" {
						body = strings.Replace(body, `"metadata":"read"`, `"metadata":"read","contents":"read"`, 1)
					}
				case "missing_permission":
					if r.Method == "POST" {
						body = strings.Replace(body, `"metadata":"read"`, `"contents":"read"`, 1)
					}
				case "extra_repository":
					if r.URL.Path == "/installation/repositories" && response.StatusCode == 200 {
						body = strings.Replace(body, `"total_count":1`, `"total_count":2`, 1)
					}
				case "next_link":
					if r.URL.Path == "/installation/repositories" && response.StatusCode == 200 {
						response.Header.Set("Link", `<https://api.github.com/installation/repositories?page=2>; rel="next"`)
					}
				case "suspended":
					body = strings.Replace(body, `"suspended_at":null`, `"suspended_at":"2026-10-29T00:00:00Z"`, 1)
				case "expiry_short":
					if r.Method == "POST" {
						body = strings.ReplaceAll(body, time.Unix(f.clock+3600, 0).UTC().Format(time.RFC3339), time.Unix(f.clock+60, 0).UTC().Format(time.RFC3339))
					}
				case "clock_rollback":
					f.clock--
				}
				response.Body = io.NopCloser(strings.NewReader(body))
				return nil
			}
			if _, err := f.issuer.execute(context.Background(), f.raw); err == nil {
				t.Fatal("invalid issuance accepted")
			}
			if _, err := os.Stat(filepath.Join(f.issuer.parent, "github-observer", "github-token")); !os.IsNotExist(err) {
				t.Fatal("invalid token published")
			}
			if name == "too_long_request" && f.posts != 0 {
				t.Fatal("insufficient pre-window sent mint")
			}
			if name == "expiry_short" && f.posts != 2 {
				t.Fatal("did not reach actual observer lifetime check")
			}
		})
	}
}

func TestObserverKeyUsesGARMFormatAndExactPublicIdentity(t *testing.T) {
	f := observerFixtureFor(t)
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(f.key)})
	raw, _ := json.Marshal(struct {
		App          int64  `json:"app_id"`
		Installation int64  `json:"installation_id"`
		Key          []byte `json:"private_key_bytes"`
	}{42, 43, private})
	pass := make([]byte, 32)
	if _, err := rand.Read(pass); err != nil {
		t.Fatal(err)
	}
	payload, err := util.Seal(raw, pass)
	if err != nil {
		t.Fatal(err)
	}
	f.plan.PayloadSHA = bytesSHA(payload)
	key, err := observerKey(f.plan, payload, pass)
	if err != nil || key.PublicKey.N.Cmp(f.key.N) != 0 {
		t.Fatal("GARM format refused", err)
	}
	signed, err := signObserver(f.plan, key, f.clock)
	if err != nil || verifyObserverJWT(f.plan, &key.PublicKey, signed, f.clock) != nil {
		t.Fatal("JWT verification")
	}
	wrong := f.plan
	wrong.Installation++
	if _, err := observerKey(wrong, payload, pass); err == nil {
		t.Fatal("cross installation accepted")
	}
	wrong = f.plan
	wrong.Fingerprint = strings.Repeat("0", 64)
	if _, err := observerKey(wrong, payload, pass); err == nil {
		t.Fatal("cross key accepted")
	}
	pass[0] ^= 1
	if _, err := observerKey(f.plan, payload, pass); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	if bytes.Contains(f.raw, private) {
		t.Fatal("private key in request")
	}
}

func TestObserverPartialPublicationRecoversWithoutMint(t *testing.T) {
	f := observerFixtureFor(t)
	failed := false
	f.issuer.files.syncHook = func(file *os.File) error {
		_, published := os.Stat(filepath.Join(f.issuer.parent, "github-observer", "github-token"))
		if filepath.Base(file.Name()) == "complete.json" && published == nil && !failed {
			failed = true
			return io.ErrUnexpectedEOF
		}
		return file.Sync()
	}
	if _, err := f.issuer.execute(context.Background(), f.raw); err == nil || !failed {
		t.Fatal("publication failure not reached")
	}
	posts, deletes, gets := f.posts, f.deletes, f.gets
	f.issuer.files.syncHook = nil
	if _, err := f.issuer.execute(context.Background(), f.raw); err != nil || f.posts != posts || f.deletes != deletes || f.gets != gets {
		t.Fatal("local publication replayed remote request", err)
	}
}

func TestObserverLostDeleteRecoversOnlyAfterActualExpiryRejection(t *testing.T) {
	f := observerFixtureFor(t)
	if _, err := f.issuer.execute(context.Background(), f.raw); err != nil {
		t.Fatal(err)
	}
	f.mutate = func(r *http.Request, _ *http.Response) error {
		if r.Method == "DELETE" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	r := observerRetirer{files: f.issuer.files, parent: f.issuer.parent, issuer: f.issuer.producer, producer: strings.Repeat("b", 64), now: f.issuer.now, fixture: f.issuer.fixture}
	if _, err := r.execute(context.Background(), f.raw); err == nil {
		t.Fatal("unknown DELETE claimed revoked")
	}
	deletes := f.deletes
	gets := f.gets
	f.mutate = nil
	if _, err := r.execute(context.Background(), f.raw); err == nil || f.deletes != deletes || f.gets != gets {
		t.Fatal("lost DELETE replayed or upgraded before expiry")
	}
	f.clock += 3601
	if _, err := r.execute(context.Background(), f.raw); err != nil || f.deletes != deletes {
		t.Fatal("expiry recovery failed or replayed DELETE", err)
	}
	raw, err := os.ReadFile(filepath.Join(f.issuer.parent, "github-observer", "retired.json"))
	if err != nil || !bytes.Contains(raw, []byte(`"observer":"expiry_rejection_observed"`)) {
		t.Fatal("expiry mislabeled revocation")
	}
}

func TestObserverLeaseMissingLockProducerExpiryAndInode(t *testing.T) {
	for _, name := range []string{"missing_lock", "producer", "expiry", "required_until", "rollback", "inode", "unsealed", "new_operation"} {
		t.Run(name, func(t *testing.T) {
			f := observerFixtureFor(t)
			if _, err := f.issuer.execute(context.Background(), f.raw); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.issuer.parent, "github-observer", "github-token")
			producer := f.issuer.producer
			switch name {
			case "missing_lock":
				if os.Remove(filepath.Join(f.issuer.parent, "session.lock")) != nil {
					t.Fatal("lock fixture")
				}
			case "producer":
				producer = strings.Repeat("b", 64)
			case "expiry":
				f.clock += 3600
			case "required_until":
				f.clock = f.plan.RequiredUntil
			case "rollback":
				f.clock--
			case "inode":
				c, err := loadGitHubToken(f.issuer.files, path, f.issuer.parent, producer, time.Unix(f.clock, 0))
				if err != nil {
					t.Fatal(err)
				}
				defer c.close()
				body, _ := os.ReadFile(path)
				if os.Rename(path, path+".original") != nil {
					t.Fatal("inode fixture")
				}
				testWrite(t, path, body, 0600)
				if (credentialTransport{credential: c, now: f.issuer.now}).valid() {
					t.Fatal("same bytes on new inode retained lease validity")
				}
				return
			case "unsealed":
				if os.Remove(filepath.Join(f.issuer.parent, "github-observer", "complete.json")) != nil {
					t.Fatal("seal fixture")
				}
			case "new_operation":
				f.plan.Operation = "cccccccc-1111-4111-8111-aaaaaaaaaaaa"
				raw, _ := json.Marshal(f.plan)
				if _, err := f.issuer.execute(context.Background(), raw); err == nil || f.posts != 2 {
					t.Fatal("new operation replaced occupied slot")
				}
				return
			}
			if c, err := loadGitHubToken(f.issuer.files, path, f.issuer.parent, producer, time.Unix(f.clock, 0)); err == nil {
				c.close()
				t.Fatal("invalid credential accepted")
			}
			if name == "missing_lock" {
				if _, err := f.issuer.execute(context.Background(), f.raw); err == nil {
					t.Fatal("missing lock repaired")
				}
				if _, err := os.Stat(filepath.Join(f.issuer.parent, "session.lock")); !os.IsNotExist(err) {
					t.Fatal("permanent lock recreated")
				}
			}
		})
	}
}

func TestObserverConsumerChecksEveryAPICall(t *testing.T) {
	f := observerFixtureFor(t)
	if _, err := f.issuer.execute(context.Background(), f.raw); err != nil {
		t.Fatal(err)
	}
	c, err := loadGitHubToken(f.issuer.files, filepath.Join(f.issuer.parent, "github-observer", "github-token"), f.issuer.parent, f.issuer.producer, time.Unix(f.clock, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	calls := 0
	tr := credentialTransport{credential: c, now: f.issuer.now, base: sessionRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		f.clock = f.plan.RequiredUntil
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/headironc/nucleus", nil)
	if _, err := tr.RoundTrip(req); err == nil || calls != 1 {
		t.Fatal("cross-window response accepted")
	}
	if _, err := tr.RoundTrip(req); err == nil || calls != 1 {
		t.Fatal("expired request sent")
	}
}

func TestObserverExternalRawCannotClaimManagedProducer(t *testing.T) {
	f := observerFixtureFor(t)
	path := filepath.Join(f.issuer.parent, "external")
	testWrite(t, path, []byte("fixture-existing-external\n"), 0600)
	c, err := loadGitHubToken(f.issuer.files, path, f.issuer.parent, "", time.Unix(f.clock, 0))
	if err != nil || c.store != nil {
		t.Fatal("external contract changed")
	}
	if c, err := loadGitHubToken(f.issuer.files, path, f.issuer.parent, f.issuer.producer, time.Unix(f.clock, 0)); err == nil {
		c.close()
		t.Fatal("external raw claimed managed issuer")
	}
}
