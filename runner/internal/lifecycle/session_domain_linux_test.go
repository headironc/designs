package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudbase/garm/auth"
	"github.com/cloudbase/garm/config"
	dbcommon "github.com/cloudbase/garm/database/common"
	"github.com/cloudbase/garm/params"
)

// Only the method used by GARM's real middleware is implemented. Unexpected
// store calls panic via the nil embedded interface rather than returning data.
type sessionDomainStore struct {
	dbcommon.Store
	user params.User
}

func (s sessionDomainStore) GetUserByID(_ context.Context, id string) (params.User, error) {
	if id != s.user.ID {
		return params.User{}, errRefused
	}
	return s.user, nil
}

type sessionDomainServer struct {
	mu      sync.Mutex
	key     string
	user    params.User
	calls   int
	force   string
	service *sessionWindowFixtureBus
}

func sessionDomainFixture(t *testing.T) (sessionDomain, []byte, []byte, gatewayFacts, *sessionDomainServer) {
	t.Helper()
	w, p, _, b := sessionWindowFixture(t)
	if w.store.reserve() != nil {
		t.Fatal("fixture window reservation")
	}
	before, err := os.ReadFile(w.installer.path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := sessionConfiguration(before, w.entropy)
	if err != nil {
		t.Fatal(err)
	}
	key, err := sessionSigningKey(before)
	if err != nil {
		t.Fatal(err)
	}
	f := &sessionDomainServer{key: key, user: params.User{ID: "eeeeeeee-0000-4000-8000-000000000005", Username: "haloadmin", Enabled: true, IsAdmin: true, Generation: 7}, service: b}
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/controller-info" {
			t.Error("domain verification attempted another endpoint")
			http.NotFound(out, request)
			return
		}
		f.mu.Lock()
		key, user, force := f.key, f.user, f.force
		f.calls++
		f.mu.Unlock()
		if force != "" {
			if force == "redirect" {
				out.Header().Set("Location", "/unexpected")
				out.WriteHeader(302)
			} else {
				out.WriteHeader(401)
				fmt.Fprint(out, force)
			}
			return
		}
		middleware, err := auth.NewjwtMiddleware(sessionDomainStore{user: user}, config.JWTAuth{Secret: key})
		if err != nil {
			t.Error(err)
			out.WriteHeader(500)
			return
		}
		middleware.Middleware(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
			if !auth.IsAdmin(request.Context()) {
				t.Error("native middleware did not bind the administrator")
				out.WriteHeader(403)
				return
			}
			out.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(out, `{"controller_id":%q,"version":"0.2.1"}`, p.Controller)
		})).ServeHTTP(out, request)
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := sessionDomain{store: w.store, plan: p, requestSHA: strings.Repeat("1", 64), user: sessionDatabase{Controller: p.Controller, User: f.user.ID, Generation: 7}, now: time.Now, fixture: fixtureRoute{server.Client().Transport, u}, services: b}
	return d, before, after, b.after, f
}

func TestSessionDomainUsesNativeGARMAuthenticationBeforeAndAfterRotation(t *testing.T) {
	d, before, after, current, f := sessionDomainFixture(t)
	if err := d.prepare(context.Background(), before); err != nil {
		t.Fatal("original key probe was not accepted by native GARM middleware")
	}
	f.mu.Lock()
	f.key, _ = sessionSigningKey(after)
	f.service.started = true
	f.mu.Unlock()
	proof, err := d.verify(context.Background(), before, after, current)
	if err != nil || !digestPattern.MatchString(proof) {
		t.Fatalf("new domain was not proven: %v", err)
	}
	if err := d.prepare(context.Background(), before); err != nil {
		t.Fatal("original probe preparation attempted authentication under the retired key")
	}
	again, err := d.verify(context.Background(), before, after, current)
	if err != nil || again != proof {
		t.Fatal("original domain proof was not reusable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != 5 {
		t.Fatalf("unexpected requests (prepare once, two GETs per verification): %d", f.calls)
	}
}

func TestSessionDomainDoesNotMistakeOtherRefusalsForRotation(t *testing.T) {
	for _, kind := range []string{"old_key", "disabled_user", "generation", "wrong_controller_invocation", "expired_probe", "malformed_unauthorized", "redirect", "null_unauthorized"} {
		t.Run(kind, func(t *testing.T) {
			d, before, after, current, f := sessionDomainFixture(t)
			if err := d.prepare(context.Background(), before); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.service.started = true
			if kind != "old_key" {
				f.key, _ = sessionSigningKey(after)
			}
			switch kind {
			case "disabled_user":
				f.user.Enabled = false
			case "generation":
				f.user.Generation++
			case "wrong_controller_invocation":
				current.Invocation = d.plan.Invocation
			case "expired_probe":
				d.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
			case "malformed_unauthorized":
				f.force = `{"error":"Authentication failed","error":"Authentication failed"}`
			case "null_unauthorized":
				f.force = `{"error":null}`
			case "redirect":
				f.force = "redirect"
			}
			f.mu.Unlock()
			if _, err := d.verify(context.Background(), before, after, current); err == nil {
				t.Fatal("non-rotation failure accepted as domain closure")
			}
			if !((sessionAcquirer{files: d.store.files}).absent(d.store, "old-domain.rejected.json")) {
				t.Fatal("failed proof produced an acceptance seal")
			}
		})
	}
}
