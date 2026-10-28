package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sessionRetirementFixture(t *testing.T, status int, body string) (sessionRetirer, sessionAcquirer, []byte, *atomic.Int32) {
	t.Helper()
	a, request, _, posts := sessionAcquisitionFixture(t)
	if _, err := a.execute(context.Background(), request); err != nil || *posts != 1 {
		t.Fatal("fixture session not acquired")
	}
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" || req.URL.Path != "/api/v1/controller-info" || req.Header.Get("Authorization") != "Bearer "+sessionJWT(sessionClaims) {
			t.Error("retirement attempted an unexpected request")
			w.WriteHeader(500)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if status == 302 {
			w.Header().Set("Location", "/unexpected")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := sessionRetirer{files: a.files, parent: a.parent, issuer: a.producer, producer: strings.Repeat("c", 64), now: func() time.Time { return time.Unix(1863216000, 0) }, fixture: fixtureRoute{server.Client().Transport, u}, check: func(_ context.Context, p sessionPlan, i sessionIntent, ready sessionWindowReady) error {
		if i.Origin.Invocation != ready.After.Invocation || p.Controller != "bbbbbbbb-0000-4000-8000-000000000002" || !ready.Listener.valid() {
			return errRefused
		}
		return nil
	}}
	return r, a, request, calls
}

const sessionDeniedBody = `{"error":"Authentication failed","details":""}`

func TestSessionRetirementRequiresConsumerExitAndPreservesOriginals(t *testing.T) {
	r, a, request, calls := sessionRetirementFixture(t, 401, sessionDeniedBody)
	path := filepath.Join(a.parent, "session", "garm-token")
	consumer, err := loadGARMToken(a.files, path, a.parent, a.producer, a.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.execute(context.Background(), request); err == nil || calls.Load() != 0 {
		t.Fatal("retirement bypassed a live consumer")
	}
	consumer.close()
	originals := map[string][]byte{}
	for _, name := range []string{"request.json", "intent.json", "response.body", "response.json", "verified.json", "complete.json"} {
		data, err := os.ReadFile(filepath.Join(a.parent, "session", name))
		if err != nil {
			t.Fatal(err)
		}
		originals[name] = data
	}
	out, err := r.execute(context.Background(), request)
	if err != nil || out.State != "expiry_rejection_observed_token_removed" || calls.Load() != 1 {
		t.Fatalf("expiry retirement failed: %+v %v", out, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("derived consumer token survived retirement")
	}
	for name, raw := range originals {
		current, err := os.ReadFile(filepath.Join(a.parent, "session", name))
		if err != nil || !bytes.Equal(raw, current) {
			t.Fatalf("retirement changed original %s", name)
		}
	}
	a.admit = func(context.Context, sessionPlan, []byte, *serviceFiles, func(sessionAdmission) error) error {
		t.Fatal("retired original attempted Login")
		return errRefused
	}
	if out, err := a.execute(context.Background(), request); err != nil || out.State != "original" {
		t.Fatalf("retired original receipt lost: %+v %v", out, err)
	}
	r.check = func(context.Context, sessionPlan, sessionIntent, sessionWindowReady) error {
		t.Fatal("original retirement requested live admission")
		return errRefused
	}
	if _, err := r.execute(context.Background(), request); err != nil || calls.Load() != 1 {
		t.Fatal("original retirement repeated a network request")
	}
}

func TestSessionRetirementRequiresExpiryTargetAndActualUnauthorizedResponse(t *testing.T) {
	for _, kind := range []string{"early", "wrong_target", "accepted", "forbidden", "failed", "redirect", "malformed", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			status, body := 401, sessionDeniedBody
			switch kind {
			case "accepted":
				status, body = 200, `{"controller_id":"bbbbbbbb-0000-4000-8000-000000000002","version":"0.2.1"}`
			case "forbidden":
				status = 403
			case "failed":
				status = 500
			case "redirect":
				status = 302
			case "malformed":
				body = `{"error":null,"details":""}`
			case "duplicate":
				body = `{"error":"Authentication failed","error":"Authentication failed","details":""}`
			}
			r, a, request, calls := sessionRetirementFixture(t, status, body)
			if kind == "early" {
				r.now = func() time.Time { return time.Unix(1863215999, 0) }
			}
			if kind == "wrong_target" {
				r.check = func(context.Context, sessionPlan, sessionIntent, sessionWindowReady) error { return errRefused }
			}
			if _, err := r.execute(context.Background(), request); err == nil {
				t.Fatal("unverified retirement accepted")
			}
			if (kind == "early" || kind == "wrong_target") && calls.Load() != 0 {
				t.Fatal("unadmitted retirement contacted server")
			}
			if _, err := os.Stat(filepath.Join(a.parent, "session", "garm-token")); err != nil {
				t.Fatal("refused retirement removed token")
			}
			if _, err := os.Stat(filepath.Join(a.parent, "session", "retired.json")); !os.IsNotExist(err) {
				t.Fatal("failed observation produced a rejection seal")
			}
			if kind != "early" && kind != "wrong_target" {
				// Once retirement starts, a clock rollback cannot reopen consumption.
				if c, err := loadGARMToken(a.files, filepath.Join(a.parent, "session", "garm-token"), a.parent, a.producer, a.now()); err == nil {
					c.close()
					t.Fatal("pending retirement reopened consumption")
				}
			}
		})
	}
}

func TestSessionRetirementRecoversLostLocalReceiptsWithoutAnotherGET(t *testing.T) {
	for _, phase := range []string{"rejection_seal", "token_unlink", "cleanup_seal"} {
		t.Run(phase, func(t *testing.T) {
			r, a, request, calls := sessionRetirementFixture(t, 401, sessionDeniedBody)
			failed := false
			r.files.syncHook = func(f *os.File) error {
				if failed || f.Name() != filepath.Join(a.parent, "session") {
					return nil
				}
				_, sealErr := os.Stat(filepath.Join(f.Name(), "retired.json"))
				_, tokenErr := os.Stat(filepath.Join(f.Name(), "garm-token"))
				_, cleanupErr := os.Stat(filepath.Join(f.Name(), "retirement.cleanup.json"))
				hit := phase == "rejection_seal" && sealErr == nil && tokenErr == nil || phase == "token_unlink" && sealErr == nil && os.IsNotExist(tokenErr) && os.IsNotExist(cleanupErr) || phase == "cleanup_seal" && cleanupErr == nil
				if hit {
					failed = true
					return errors.New("fixture lost directory sync")
				}
				return nil
			}
			if _, err := r.execute(context.Background(), request); err == nil || !failed || calls.Load() != 1 {
				t.Fatal("fixture did not interrupt local publication")
			}
			r.files.syncHook = nil
			r.check = func(context.Context, sessionPlan, sessionIntent, sessionWindowReady) error {
				t.Fatal("retained rejection called live target")
				return errRefused
			}
			if _, err := r.execute(context.Background(), request); err != nil || calls.Load() != 1 {
				t.Fatalf("original retirement recovery failed: %v", err)
			}
		})
	}
}

func TestSessionRetirementPreservesReplacementConsumerFile(t *testing.T) {
	r, a, request, _ := sessionRetirementFixture(t, 401, sessionDeniedBody)
	interrupted := false
	r.files.syncHook = func(f *os.File) error {
		if !interrupted && f.Name() == filepath.Join(a.parent, "session") {
			if _, err := os.Stat(filepath.Join(f.Name(), "retired.json")); err == nil {
				interrupted = true
				return errRefused
			}
		}
		return nil
	}
	if _, err := r.execute(context.Background(), request); err == nil || !interrupted {
		t.Fatal("fixture did not stop before unlink")
	}
	r.files.syncHook = nil
	path := filepath.Join(a.parent, "session", "garm-token")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.Rename(path, path+".original") != nil {
		t.Fatal("fixture rename")
	}
	testWrite(t, path, raw, 0600)
	if _, err := r.execute(context.Background(), request); err == nil {
		t.Fatal("replacement inode accepted for deletion")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("foreign replacement was deleted")
	}
}
