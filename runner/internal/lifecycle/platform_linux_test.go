package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudbase/garm/params"
	"github.com/google/go-github/v84/github"
)

type fixtureRoute struct {
	base http.RoundTripper
	url  *url.URL
}

func (t fixtureRoute) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = t.url.Scheme
	u.Host = t.url.Host
	clone.URL = &u
	return t.base.RoundTrip(clone)
}

func platformBodies() map[string]string {
	repo := `{"id":9007199254740993,"name":"consumer","full_name":"example/consumer","owner":{"login":"example"}}`
	return map[string]string{
		"/repos/example/consumer": repo,
		"/repos/example/consumer/actions/runs/9007199254740995/attempts/2": `{"id":9007199254740995,"run_attempt":2,"head_sha":"0123456789012345678901234567890123456789","status":"in_progress","repository":` + repo + `}`,
		"/repos/example/consumer/actions/jobs/9007199254740997":            `{"id":9007199254740997,"run_id":9007199254740995,"run_attempt":2,"head_sha":"0123456789012345678901234567890123456789","status":"in_progress","runner_id":9007199254740999,"runner_name":"runner-one"}`,
		"/api/v1/controller-info":                                          `{"controller_id":"bbbbbbbb-0000-4000-8000-000000000002"}`,
		"/api/v1/repositories/cccccccc-0000-4000-8000-000000000003":        `{"id":"cccccccc-0000-4000-8000-000000000003","owner":"example","name":"consumer","endpoint":{"endpoint_type":"github","base_url":"https://github.com","api_base_url":"https://api.github.com/"}}`,
		"/api/v1/scalesets/27":                                             `{"id":27,"scale_set_id":99,"name":"build","provider_name":"halo-lxd","repo_id":"cccccccc-0000-4000-8000-000000000003","endpoint":{"endpoint_type":"github","base_url":"https://github.com/","api_base_url":"https://api.github.com"}}`,
		"/api/v1/instances/runner-one":                                     `{"id":"dddddddd-0000-4000-8000-000000000004","name":"runner-one","provider_id":"runner-one","provider_name":"halo-lxd","agent_id":9007199254740999,"scale_set_id":27,"status":"running","runner_status":"active"}`,
	}
}

func platformFixture(t *testing.T, bodies map[string]string) *platforms {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("SDK attempted write")
			http.Error(w, "write", 500)
			return
		}
		expected := "Bearer fixture-github-secret"
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			expected = "Bearer fixture-garm-secret"
		}
		if r.Header.Get("Authorization") != expected {
			t.Error("credential did not reach intended fixture")
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			t.Errorf("unexpected SDK path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlatforms("fixture-github-secret", "fixture-garm-secret", fixtureRoute{server.Client().Transport, u})
	t.Cleanup(p.close)
	return p
}

func TestPlatformSDKExactIdentities(t *testing.T) {
	p := platformFixture(t, platformBodies())
	r, err := parseRequest([]byte(requestJSON))
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.observe(context.Background(), r, binding())
	if err != nil || id != 99 {
		t.Fatalf("ScaleSet %d, %v", id, err)
	}
}

func TestPlatformSDKRejectsUnicodeFieldAliases(t *testing.T) {
	job := "/repos/example/consumer/actions/jobs/9007199254740997"
	repo := "/repos/example/consumer"
	for _, tc := range []struct{ name, path, old, replacement string }{
		{"long-s", job, `"status":"in_progress"`, `"status":"completed","ſtatus":"in_progress"`},
		{"kelvin", repo, `"name":"consumer"`, `"name":"consumer","fork":false,"forK":true`},
		{"nested", repo, `"login":"example"`, `"login":"example","site_admin":false,"ſite_admin":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := platformBodies()
			bodies[tc.path] = strings.Replace(bodies[tc.path], tc.old, tc.replacement, 1)
			if tc.path == job {
				var decoded github.WorkflowJob
				if json.Unmarshal([]byte(bodies[tc.path]), &decoded) != nil || decoded.Status == nil || *decoded.Status != "in_progress" {
					t.Fatal("SDK did not recognize long-s alias")
				}
			} else {
				var decoded github.Repository
				if json.Unmarshal([]byte(bodies[tc.path]), &decoded) != nil {
					t.Fatal("invalid repository fixture")
				}
				if tc.name == "kelvin" && (decoded.Fork == nil || !*decoded.Fork) {
					t.Fatal("SDK did not recognize Kelvin alias")
				}
				if tc.name == "nested" && (decoded.Owner == nil || decoded.Owner.SiteAdmin == nil || !*decoded.Owner.SiteAdmin) {
					t.Fatal("SDK did not recognize nested alias")
				}
			}
			p := platformFixture(t, bodies)
			r, _ := parseRequest([]byte(requestJSON))
			if _, err := p.observe(context.Background(), r, binding()); err == nil {
				t.Fatal("SDK accepted duplicate Unicode field aliases")
			}
		})
	}
}

func TestPlatformSDKPreservesOrdinaryUnicodeMapKeys(t *testing.T) {
	bodies := platformBodies()
	path := "/repos/example/consumer"
	bodies[path] = strings.Replace(bodies[path], `"name":"consumer"`, `"name":"consumer","custom_properties":{"环境":"测试","é":"accent","e":"plain","日本語":"値"}`, 1)
	var decoded github.Repository
	if json.Unmarshal([]byte(bodies[path]), &decoded) != nil || decoded.CustomProperties["环境"] != "测试" || decoded.CustomProperties["é"] != "accent" || decoded.CustomProperties["e"] != "plain" {
		t.Fatal("SDK lost ordinary Unicode map keys")
	}
	p := platformFixture(t, bodies)
	r, _ := parseRequest([]byte(requestJSON))
	if _, err := p.observe(context.Background(), r, binding()); err != nil {
		t.Fatal(err)
	}
}

func TestPublicEndpointProducerContract(t *testing.T) {
	for _, base := range []string{"https://github.com", "https://github.com/"} {
		for _, api := range []string{"https://api.github.com", "https://api.github.com/"} {
			if !publicEndpoint(params.ForgeEndpoint{EndpointType: params.GithubEndpointType, BaseURL: base, APIBaseURL: api}) {
				t.Fatal("public endpoint rejected")
			}
		}
	}
	for _, bad := range []string{"", "https://evil.invalid", "http://github.com", "https://github.com/api", "https://user@github.com", "https://github.com?x=1", "https://github.com?", "https://github.com/#fragment", "https://github.com:443"} {
		if publicEndpoint(params.ForgeEndpoint{EndpointType: params.GithubEndpointType, BaseURL: bad, APIBaseURL: "https://api.github.com/"}) {
			t.Fatalf("unsafe base %s", bad)
		}
	}
	if publicEndpoint(params.ForgeEndpoint{EndpointType: params.GiteaEndpointType, BaseURL: "https://github.com", APIBaseURL: "https://api.github.com/"}) || publicEndpoint(params.ForgeEndpoint{EndpointType: params.GithubEndpointType, BaseURL: "https://github.com"}) {
		t.Fatal("unknown forge/absent API accepted")
	}
}

func TestPlatformRejectsWrongMissingAndNullFacts(t *testing.T) {
	job := "/repos/example/consumer/actions/jobs/9007199254740997"
	run := "/repos/example/consumer/actions/runs/9007199254740995/attempts/2"
	for _, tc := range []struct{ name, path, field, value string }{
		{"job", job, "id", "9007199254740996"}, {"run", job, "run_id", "9007199254740996"}, {"attempt", job, "run_attempt", "1"}, {"runner", job, "runner_id", "8"}, {"runner name", job, "runner_name", `"runner-other"`}, {"commit", job, "head_sha", `"wrong"`}, {"completed", job, "status", `"completed"`},
		{"run attempt", run, "run_attempt", "3"}, {"run repository", run, "repository", `{"id":1}`}, {"repo", "/repos/example/consumer", "id", "1"},
		{"controller", "/api/v1/controller-info", "controller_id", `"00000000-0000-4000-8000-000000000000"`},
		{"garm repo", "/api/v1/repositories/cccccccc-0000-4000-8000-000000000003", "owner", `"other"`},
		{"garm scale", "/api/v1/scalesets/27", "id", "28"}, {"github scale", "/api/v1/scalesets/27", "scale_set_id", "0"}, {"garm scale repo", "/api/v1/scalesets/27", "repo_id", `"00000000-0000-4000-8000-000000000000"`}, {"garm provider", "/api/v1/scalesets/27", "provider_name", `"other"`},
		{"garm instance", "/api/v1/instances/runner-one", "id", `"00000000-0000-4000-8000-000000000000"`}, {"agent", "/api/v1/instances/runner-one", "agent_id", "42"}, {"instance scaleset", "/api/v1/instances/runner-one", "scale_set_id", "28"}, {"inactive", "/api/v1/instances/runner-one", "runner_status", `"idle"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := platformBodies()
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(bodies[tc.path]), &body); err != nil {
				t.Fatal(err)
			}
			body[tc.field] = json.RawMessage(tc.value)
			raw, _ := json.Marshal(body)
			bodies[tc.path] = string(raw)
			p := platformFixture(t, bodies)
			r, _ := parseRequest([]byte(requestJSON))
			if _, err := p.observe(context.Background(), r, binding()); err == nil {
				t.Fatal("accepted wrong fact")
			}
		})
	}
	// Every required GitHub pointer is independently checked for absence/null.
	for path, fields := range map[string][]string{job: {"id", "run_id", "run_attempt", "head_sha", "status", "runner_id", "runner_name"}, run: {"id", "run_attempt", "head_sha", "status", "repository"}, "/repos/example/consumer": {"id", "name", "full_name", "owner"}} {
		for _, field := range fields {
			for _, value := range []string{"missing", "null"} {
				t.Run(field+" "+value+path, func(t *testing.T) {
					bodies := platformBodies()
					var body map[string]json.RawMessage
					json.Unmarshal([]byte(bodies[path]), &body)
					if value == "missing" {
						delete(body, field)
					} else {
						body[field] = json.RawMessage("null")
					}
					raw, _ := json.Marshal(body)
					bodies[path] = string(raw)
					p := platformFixture(t, bodies)
					r, _ := parseRequest([]byte(requestJSON))
					if _, err := p.observe(context.Background(), r, binding()); err == nil {
						t.Fatal("accepted absent GitHub fact")
					}
				})
			}
		}
	}
	for _, bad := range []string{`null`, `{}`, `{"id":1,"ID":2}`, strings.Repeat(" ", 2*1024*1024+1), `{"id":9223372036854775808}`, `{"id":1.5}`} {
		t.Run(fmt.Sprintf("malformed-%d", len(bad)), func(t *testing.T) {
			bodies := platformBodies()
			bodies[job] = bad
			p := platformFixture(t, bodies)
			r, _ := parseRequest([]byte(requestJSON))
			if _, err := p.observe(context.Background(), r, binding()); err == nil {
				t.Fatal("accepted malformed response")
			}
		})
	}
}

func TestReadTransportRejectsWriteRedirectAndEndpointEscape(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "http://untrusted.invalid/secret")
		w.WriteHeader(302)
		io.WriteString(w, "fixture-secret-must-not-be-returned")
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	transport := readTransport{fixtureRoute{server.Client().Transport, u}, "https", "api.github.com"}
	for _, tc := range []struct{ method, url string }{{"POST", "https://api.github.com/repos/example/consumer"}, {"GET", "https://evil.invalid/repos/example/consumer"}, {"GET", "http://api.github.com/repos/example/consumer"}, {"GET", "https://api.github.com/repos/example/consumer"}} {
		req, _ := http.NewRequest(tc.method, tc.url, nil)
		_, err := transport.RoundTrip(req)
		if err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatal("unsafe transport outcome")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("endpoint guard made %d calls", calls.Load())
	}
}
