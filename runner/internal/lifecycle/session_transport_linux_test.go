package lifecycle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sessionClaims = `{"user":"bbbbbbbb-0000-4000-8000-000000000002","token_id":"abcdefghijklmnop","full_name":"Fixture Admin","is_admin":true,"read_metrics":false,"generation":0,"exp":1863216000,"iss":"garm"}`

func sessionJWT(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(claims)) + "." + enc(make([]byte, 32))
}

func sessionFixture(t *testing.T, handler http.HandlerFunc, retain func(int, []byte) error) sessionClient {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := newBoundSessionClient(retain, fixtureRoute{s.Client().Transport, u}, nil)
	t.Cleanup(c.close)
	return c
}

func TestSessionSDKRetainsResponseBeforeConsumption(t *testing.T) {
	var posts, gets int
	var retained []byte
	controller := "bbbbbbbb-0000-4000-8000-000000000002"
	token := sessionJWT(sessionClaims)
	body := fmt.Sprintf(`{"token":%q}`, token)
	c := sessionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			posts++
			var request map[string]string
			if r.Method != "POST" || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&request) != nil || len(request) != 2 || request["username"] != "haloadmin" || request["password"] != "fixture-password" {
				t.Error("wrong SDK login request")
			}
			io.WriteString(w, body)
		case "/api/v1/controller-info":
			gets++
			if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+token || string(retained) != body {
				t.Error("authenticated read preceded retained response or used wrong token")
			}
			fmt.Fprintf(w, `{"controller_id":%q,"version":"0.2.1"}`, controller)
		default:
			t.Error("unexpected SDK route")
			http.NotFound(w, r)
		}
	}, func(status int, raw []byte) error {
		if status != 200 {
			t.Error("wrong retained status")
		}
		retained = append([]byte(nil), raw...)
		return nil
	})
	got, err := c.login(context.Background(), "haloadmin", "fixture-password")
	if err != nil || got != token || string(retained) != body {
		t.Fatalf("login did not return its retained original: %v", err)
	}
	if c.controller(context.Background(), got, controller) != nil {
		t.Fatal("bound controller rejected")
	}
	if _, err := c.login(context.Background(), "haloadmin", "fixture-password"); err == nil || posts != 1 || gets != 1 {
		t.Fatal("one intent sent a second login")
	}
}

func TestSessionLoginFailureNeverRetriesOrReturnsSecrets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		store  bool
	}{
		{"status", 401, `{"error":"fixture-secret"}`, true},
		{"redirect", 307, `{"token":"fixture-secret"}`, true},
		{"missing", 200, `{}`, true},
		{"null", 200, `{"token":null}`, true},
		{"alias", 200, `{"Token":"fixture-secret"}`, true},
		{"duplicate", 200, `{"token":"first","token":"fixture-secret"}`, true},
		{"trailing", 200, `{"token":"fixture-secret"} {}`, true},
		{"oversize", 200, strings.Repeat("x", 64*1024+1), false},
		{"persistence", 200, `{"token":"fixture-secret"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts, saves := 0, 0
			c := sessionFixture(t, func(w http.ResponseWriter, r *http.Request) {
				posts++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Location", "http://127.0.0.1:9997/api/v1/auth/login")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}, func(status int, raw []byte) error {
				saves++
				if status != tc.status || string(raw) != tc.body {
					t.Error("original response changed before persistence")
				}
				if tc.name == "persistence" {
					return errors.New("fixture-secret persistence failure")
				}
				return nil
			})
			for i := 0; i < 2; i++ {
				token, err := c.login(context.Background(), "haloadmin", "fixture-password")
				if err == nil || token != "" || strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "fixture-password") {
					t.Fatal("failure returned a credential or unsafe error")
				}
			}
			want := 0
			if tc.store {
				want = 1
			}
			if posts != 1 || saves != want {
				t.Fatalf("posts=%d saves=%d; want 1/%d", posts, saves, want)
			}
		})
	}
}

func TestSessionConcurrentCallsSendOnePost(t *testing.T) {
	var calls atomic.Int32
	c := sessionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":%q}`, sessionJWT(sessionClaims))
	}, func(int, []byte) error { return nil })
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = c.login(ctx, "haloadmin", "fixture-password")
		}()
	}
	close(start)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent SDK calls repeated the login")
	}
}

func TestSessionExpiryUsesExactGARMClaimsAndRequestInterval(t *testing.T) {
	started := time.Unix(1863129600, 0)
	ended := started.Add(2 * time.Second)
	if expiry, err := sessionExpiry(sessionJWT(sessionClaims), started, ended); err != nil || expiry != 1863216000 {
		t.Fatalf("original GARM claims rejected: %v", err)
	}
	for _, claims := range []string{
		strings.Replace(sessionClaims, `"exp":1863216000`, `"exp":1863302400`, 1),
		strings.Replace(sessionClaims, `"exp":1863216000`, `"exp":1863215999`, 1),
		strings.Replace(sessionClaims, `"exp":1863216000`, `"exp":1863216000.5`, 1),
		strings.Replace(sessionClaims, `"exp":1863216000`, `"exp":null`, 1),
		strings.Replace(sessionClaims, `"is_admin":true`, `"is_admin":false`, 1),
		strings.Replace(sessionClaims, `"generation":0,`, ``, 1),
		strings.Replace(sessionClaims, `"generation":0`, `"generation":-1`, 1),
		strings.Replace(sessionClaims, `"iss":"garm"`, `"iss":"other"`, 1),
		strings.Replace(sessionClaims, `"read_metrics":false`, `"read_metrics":true`, 1),
		strings.Replace(sessionClaims, `"exp":1863216000`, `"exp":1863216000,"exp":1863216000`, 1),
	} {
		if _, err := sessionExpiry(sessionJWT(claims), started, ended); err == nil {
			t.Fatal("invalid claims accepted")
		}
	}
	for _, token := range []string{"not-jwt", sessionJWT(sessionClaims) + "=", "." + strings.Split(sessionJWT(sessionClaims), ".")[1] + ".x"} {
		if _, err := sessionExpiry(token, started, ended); err == nil {
			t.Fatal("invalid JWT encoding accepted")
		}
	}
}
