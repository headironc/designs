package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type sessionAdmissionFixture struct {
	passwords int
	closed    bool
	fail      bool
	source    sessionOrigin
}

func (a *sessionAdmissionFixture) origin() sessionOrigin { return a.source }

func (a *sessionAdmissionFixture) check(context.Context) error {
	if a.fail {
		return errRefused
	}
	return nil
}
func (a *sessionAdmissionFixture) password() (string, error) {
	a.passwords++
	return "fixture-password", nil
}
func (a *sessionAdmissionFixture) close() { a.closed = true }

func sessionAcquisitionFixture(t *testing.T) (sessionAcquirer, []byte, *sessionAdmissionFixture, *int) {
	t.Helper()
	root := t.TempDir()
	parent := filepath.Join(root, "credentials")
	if os.Mkdir(parent, 0700) != nil {
		t.Fatal("fixture directory")
	}
	posts := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/login" {
			*posts++
			fmt.Fprintf(w, `{"token":%q}`, sessionJWT(sessionClaims))
		} else if r.URL.Path == "/api/v1/controller-info" && r.Header.Get("Authorization") == "Bearer "+sessionJWT(sessionClaims) {
			io.WriteString(w, `{"controller_id":"bbbbbbbb-0000-4000-8000-000000000002","version":"0.2.1"}`)
		} else {
			t.Error("unapproved fixture request")
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	admission := &sessionAdmissionFixture{}
	a := sessionAcquirer{
		files:    protectedFiles{uid: uint32(os.Getuid()), boundary: root},
		parent:   parent,
		producer: strings.Repeat("a", 64),
		admit: func(_ context.Context, _ sessionPlan, _ []byte, _ *serviceFiles, consume func(sessionAdmission) error) error {
			return consume(admission)
		},
		fixture: fixtureRoute{server.Client().Transport, u},
		now:     func() time.Time { return time.Unix(1863129600, 0) },
	}
	w, p, raw, _ := sessionWindowFixture(t)
	w.producer = a.producer
	ready, err := w.execute(context.Background(), p, raw)
	if err != nil {
		t.Fatal("fixture credential window did not complete")
	}
	admission.source = sessionOrigin{WindowSHA: bytesSHA(w.store.raw["window.ready.json"]), Invocation: ready.After.Invocation, ConfigSHA: ready.ConfigSHA, User: p.Controller, Generation: 0}
	if !admission.source.valid(p) || a.verifyWindow(w.store, p, admission.source, bytesSHA(raw), a.producer) != nil {
		t.Fatal("fixture window is not readable by its credential consumer")
	}
	a.files, a.parent = w.store.files, filepath.Dir(w.store.path)
	w.store.close()
	return a, raw, admission, posts
}

func TestSessionAcquisitionPublishesCompatibleTokenAndRecoversOriginal(t *testing.T) {
	a, raw, admission, posts := sessionAcquisitionFixture(t)
	out, err := a.execute(context.Background(), raw)
	if err != nil || out.State != "acquired" || out.Expires != 1863216000 || *posts != 1 || admission.passwords != 1 || !admission.closed {
		t.Fatalf("acquisition failed: %#v %v posts=%d password_reads=%d", out, err, *posts, admission.passwords)
	}
	file := filepath.Join(a.parent, "session", "garm-token")
	token, err := a.files.read(file, 0600, 64*1024+1)
	if err != nil || string(token) != sessionJWT(sessionClaims)+"\n" || !validToken(strings.TrimSpace(string(token))) {
		t.Fatal("published token violates existing Go consumer contract")
	}
	// Original readback must not need the password, current service or network.
	a.admit = func(context.Context, sessionPlan, []byte, *serviceFiles, func(sessionAdmission) error) error {
		t.Fatal("original result attempted live admission")
		return errRefused
	}
	a.fixture = nil
	out, err = a.execute(context.Background(), raw)
	if err != nil || out.State != "original" || *posts != 1 || admission.passwords != 1 {
		t.Fatalf("original result not recovered: %#v %v", out, err)
	}
	changed := []byte(strings.Replace(string(raw), "aaaaaaaa-0000", "cccccccc-0000", 1))
	if _, err := a.execute(context.Background(), changed); err == nil {
		t.Fatal("new operation bypassed occupied global slot")
	}
}

func TestSessionUnknownSendNeverReplays(t *testing.T) {
	a, raw, admission, posts := sessionAcquisitionFixture(t)
	a.fixture = sessionRoundTripFunc(func(*http.Request) (*http.Response, error) {
		*posts++
		return nil, errors.New("fixture lost response")
	})
	for i := 0; i < 2; i++ {
		out, err := a.execute(context.Background(), raw)
		if err == nil || out.State != "unknown" {
			t.Fatal("unknown request became a success")
		}
	}
	if *posts != 1 || admission.passwords != 1 {
		t.Fatal("unknown request reread password or repeated Login")
	}
}

type sessionRoundTripFunc func(*http.Request) (*http.Response, error)

func (f sessionRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionCompleteWriteFailureRecoversWithoutAnotherLogin(t *testing.T) {
	a, raw, admission, posts := sessionAcquisitionFixture(t)
	a.files.syncHook = func(f *os.File) error {
		if strings.HasSuffix(f.Name(), "complete.json") {
			return errors.New("fixture seal synchronization unknown")
		}
		return f.Sync()
	}
	if out, err := a.execute(context.Background(), raw); err == nil || out.State != "unknown" {
		t.Fatal("failed completion synchronization reported success")
	}
	a.files.syncHook = nil
	out, err := a.execute(context.Background(), raw)
	if err != nil || out.State != "original" || *posts != 1 || admission.passwords != 1 {
		t.Fatalf("original completion not recovered: %#v %v", out, err)
	}
}

func TestSessionMissingSealResumesOnlyOriginalResponse(t *testing.T) {
	a, raw, admission, posts := sessionAcquisitionFixture(t)
	// Cancel admission after the response is durable, before publication.
	base := a.fixture
	a.fixture = sessionRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := base.RoundTrip(r)
		if r.Method == "POST" {
			admission.fail = true
		}
		return response, err
	})
	if _, err := a.execute(context.Background(), raw); err == nil {
		t.Fatal("changed admission allowed publication")
	}
	admission.fail = false
	out, err := a.execute(context.Background(), raw)
	if err != nil || out.State != "acquired" || *posts != 1 || admission.passwords != 1 {
		t.Fatalf("original response could not finish publication: %#v %v", out, err)
	}
}

func TestSessionAdmissionRefusalPreventsReservationAndSend(t *testing.T) {
	a, raw, admission, posts := sessionAcquisitionFixture(t)
	admission.fail = true
	if _, err := a.execute(context.Background(), raw); err == nil || *posts != 0 || admission.passwords != 0 {
		t.Fatal("refused admission sent Login")
	}
	if _, err := os.Lstat(filepath.Join(a.parent, "session")); !os.IsNotExist(err) {
		t.Fatal("refused admission occupied an operation")
	}
}

func TestSessionConcurrentGlobalSlotWritersSendOnce(t *testing.T) {
	a, raw, admission, posts := sessionAcquisitionFixture(t)
	// Each contender owns a distinct admission and transport. The kernel lock
	// and fixed slot, rather than the transport's single-use flag, serialize it.
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			contender := a
			contender.admit = func(_ context.Context, _ sessionPlan, _ []byte, _ *serviceFiles, consume func(sessionAdmission) error) error {
				return consume(&sessionAdmissionFixture{source: admission.source})
			}
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = contender.execute(ctx, raw)
		}()
	}
	close(start)
	group.Wait()
	if *posts != 1 {
		t.Fatal("global slot allowed more than one Login")
	}
}

func TestSessionAdmissionOwnsOneSynchronousAcquisition(t *testing.T) {
	a, raw, original, posts := sessionAcquisitionFixture(t)
	a.admit = func(_ context.Context, _ sessionPlan, _ []byte, store *serviceFiles, consume func(sessionAdmission) error) error {
		if !store.unchanged() {
			t.Fatal("admission did not inherit the global credential lease")
		}
		first := &sessionAdmissionFixture{source: original.source}
		if err := consume(first); err != nil || !first.closed {
			t.Fatal("acquisition escaped its admission lifetime")
		}
		if err := consume(&sessionAdmissionFixture{}); err == nil {
			t.Fatal("admission callback initiated a second acquisition")
		}
		return nil
	}
	if _, err := a.execute(context.Background(), raw); err != nil || *posts != 1 {
		t.Fatal("synchronous admission did not preserve one Login")
	}
}

func TestSessionAcquisitionRequiresCompletedBoundWindow(t *testing.T) {
	for _, name := range []string{"window.intent.json", "window.ready.json", "watchdog.event.json", "watchdog.drained.json", "controller.stop.job.json", "controller.stopped.json", "controller.start.event.json", "controller.started.json", "old-domain.rejected.json", "configuration.after.toml"} {
		t.Run(name, func(t *testing.T) {
			a, raw, _, posts := sessionAcquisitionFixture(t)
			path := filepath.Join(a.parent, "window", name)
			if err := os.Rename(path, path+".fixture"); err != nil {
				t.Fatal(err)
			}
			if _, err := a.execute(context.Background(), raw); err == nil || *posts != 0 {
				t.Fatal("missing original window evidence permitted Login")
			}
			if _, err := os.Stat(filepath.Join(a.parent, "session")); !os.IsNotExist(err) {
				t.Fatal("refused window reserved another login")
			}
		})
	}
}

func TestSessionReadbackRequiresOriginalWindowAndPrincipal(t *testing.T) {
	a, raw, admission, posts := sessionAcquisitionFixture(t)
	if _, err := a.execute(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	admission.source.Invocation = strings.Repeat("3", 32)
	file := filepath.Join(a.parent, "window", "controller.start.event.json")
	if err := os.Rename(file, file+".fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.execute(context.Background(), raw); err == nil || *posts != 1 {
		t.Fatal("readback accepted lost window provenance or resent Login")
	}
	if err := os.Rename(file+".fixture", file); err != nil {
		t.Fatal(err)
	}
	if out, err := a.execute(context.Background(), raw); err != nil || out.State != "original" || *posts != 1 {
		t.Fatal("original private window could not recover the original result")
	}
	for _, claims := range []string{
		strings.Replace(sessionClaims, `"user":"bbbbbbbb-0000-4000-8000-000000000002"`, `"user":"ffffffff-0000-4000-8000-000000000006"`, 1),
		strings.Replace(sessionClaims, `"generation":0`, `"generation":1`, 1),
	} {
		if admission.source.principal(sessionJWT(claims)) {
			t.Fatal("a different database principal matched the admitted identity")
		}
	}
}

func TestSessionOriginalRejectsCorruptResponse(t *testing.T) {
	a, raw, _, posts := sessionAcquisitionFixture(t)
	if _, err := a.execute(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(a.parent, "session", "response.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var r sessionResponse
	if json.Unmarshal(data, &r) != nil {
		t.Fatal("fixture response")
	}
	r.Status = 401
	data, _ = json.Marshal(r)
	if os.WriteFile(file, data, 0600) != nil {
		t.Fatal("fixture mutation")
	}
	if _, err := a.execute(context.Background(), raw); err == nil || *posts != 1 {
		t.Fatal("corrupt response accepted or retried")
	}
}
