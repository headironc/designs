package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudbase/garm/params"
	"github.com/headironc/designs/runner/internal/provider"
)

func originalFixture(t *testing.T) receipt {
	t.Helper()
	c, path, operations := captureFixture(t)
	if stage, err := c.execute(context.Background(), path, operations); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	r, _ := parseRequest([]byte(requestJSON))
	var result receipt
	ok, err := c.files.originalRecord(operations, r, []byte(requestJSON), strings.Repeat("6", 64), &result)
	if err != nil || !ok {
		t.Fatalf("original: %v", err)
	}
	return result
}

func diagnosticBodies() map[string]string {
	b := platformBodies()
	b["/repos/example/consumer/actions/runners"] = `{"total_count":1,"runners":[{"id":9007199254740999,"name":"runner-one"}]}`
	b["/api/v1/instances"] = "[" + b["/api/v1/instances/runner-one"] + "]"
	b["/api/v1/scalesets/27/instances"] = b["/api/v1/instances"]
	return b
}

func TestDiagnosticJobConclusionsAndRunningRun(t *testing.T) {
	r := originalFixture(t)
	for _, conclusion := range []string{"success", "failure", "cancelled", "timed_out"} {
		t.Run(conclusion, func(t *testing.T) {
			b := diagnosticBodies()
			path := "/repos/example/consumer/actions/jobs/9007199254740997"
			b[path] = strings.Replace(b[path], `"status":"in_progress"`, `"status":"completed","conclusion":"`+conclusion+`"`, 1)
			p := platformFixture(t, b)
			got := p.diagnoseJob(context.Background(), r)
			if got.State != "terminal" || got.Conclusion != conclusion || got.RunState != "in_progress" {
				t.Fatalf("lost actual outcome: %+v", got)
			}
		})
	}
	for _, mutation := range []string{`"run_attempt":3`, `"run_id":1`, `"runner_id":1`, `"head_sha":"bad"`} {
		b := diagnosticBodies()
		path := "/repos/example/consumer/actions/jobs/9007199254740997"
		var row map[string]json.RawMessage
		json.Unmarshal([]byte(b[path]), &row)
		var change map[string]json.RawMessage
		json.Unmarshal([]byte("{"+mutation+"}"), &change)
		for k, v := range change {
			row[k] = v
		}
		raw, _ := json.Marshal(row)
		b[path] = string(raw)
		if got := platformFixture(t, b).diagnoseJob(context.Background(), r); got.State != "mismatch" {
			t.Fatalf("wrong original accepted: %+v", got)
		}
	}
}

func TestDiagnosticGARMProducerEnabledAndIdentities(t *testing.T) {
	r := originalFixture(t)
	for _, enabled := range []bool{false, true} {
		b := diagnosticBodies()
		var scale params.ScaleSet
		if json.Unmarshal([]byte(b["/api/v1/scalesets/27"]), &scale) != nil {
			t.Fatal("fixture")
		}
		scale.Enabled = enabled
		raw, _ := json.Marshal(scale)
		if !enabled && bytes.Contains(raw, []byte(`"enabled"`)) {
			t.Fatal("actual producer no longer omits false")
		}
		b["/api/v1/scalesets/27"] = string(raw)
		got := platformFixture(t, b).diagnoseGARM(context.Background(), r)
		want := "disabled_observed"
		if enabled {
			want = "enabled_observed"
		}
		if got.State != "present" || got.Admission != want {
			t.Fatalf("producer contract: %+v", got)
		}
	}
	for _, bad := range []string{"null", `"false"`, `1`} {
		b := diagnosticBodies()
		path := "/api/v1/scalesets/27"
		b[path] = strings.TrimSuffix(b[path], "}") + `,"enabled":` + bad + `}`
		if got := platformFixture(t, b).diagnoseGARM(context.Background(), r); got.State != "unknown" {
			t.Fatalf("invalid bool: %+v", got)
		}
	}
	for _, tc := range []struct{ path, old, new string }{
		{"/api/v1/scalesets/27", `"id":27`, `"id":28`},
		{"/api/v1/scalesets/27", `"repo_id":`, `"org_id":"other","repo_id":`},
		{"/api/v1/scalesets/27", `"repo_id":`, `"enterprise_id":"other","repo_id":`},
		{"/api/v1/scalesets/27/instances", `"provider_id":"runner-one"`, `"provider_id":"other"`},
		{"/api/v1/scalesets/27/instances", `"provider_name":"halo-lxd"`, `"provider_name":"other"`},
		{"/api/v1/instances", `9007199254740999`, `1`},
		{"/api/v1/scalesets/27/instances", `[`, `[null,`},
	} {
		b := diagnosticBodies()
		b[tc.path] = strings.Replace(b[tc.path], tc.old, tc.new, 1)
		got := platformFixture(t, b).diagnoseGARM(context.Background(), r)
		if got.State == "present" || got.State == "absent_verified" {
			t.Fatalf("identity accepted: %+v", got)
		}
	}
	b := diagnosticBodies()
	b["/api/v1/instances"] = "[]"
	b["/api/v1/scalesets/27/instances"] = "[]"
	if got := platformFixture(t, b).diagnoseGARM(context.Background(), r); got.State != "absent_verified" {
		t.Fatalf("complete absence: %+v", got)
	}
}

func TestDiagnosticRegistrationPagination(t *testing.T) {
	r := originalFixture(t)
	for _, mode := range []string{"complete", "duplicate", "missing_total", "null_total", "null_list", "incomplete", "changed_total", "replacement", "404"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.Method != "GET" || q.URL.Query().Get("per_page") != "100" {
					t.Error("unbounded/write SDK request")
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "404" {
					http.Error(w, "fixture-sensitive-error", 404)
					return
				}
				if q.URL.Query().Get("page") == "1" {
					w.Header().Set("Link", `<https://api.github.com/repos/example/consumer/actions/runners?page=2&per_page=100>; rel="next"`)
					io.WriteString(w, `{"total_count":2,"runners":[{"id":1,"name":"other"}]}`)
					return
				}
				body := `{"total_count":2,"runners":[{"id":9007199254740999,"name":"runner-one"}]}`
				switch mode {
				case "duplicate":
					body = `{"total_count":2,"runners":[{"id":1,"name":"other"}]}`
				case "missing_total":
					body = `{"runners":[]}`
				case "null_total":
					body = `{"total_count":null,"runners":[]}`
				case "null_list":
					body = `{"total_count":2,"runners":null}`
				case "incomplete":
					body = `{"total_count":2,"runners":[]}`
				case "changed_total":
					body = strings.Replace(body, `"total_count":2`, `"total_count":3`, 1)
				case "replacement":
					body = strings.Replace(body, `9007199254740999`, `2`, 1)
				}
				io.WriteString(w, body)
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			p := newPlatforms("fixture", "fixture", fixtureRoute{server.Client().Transport, u})
			defer p.close()
			got := p.diagnoseRegistration(context.Background(), r)
			want := "unknown"
			if mode == "complete" {
				want = "present"
			}
			if mode == "replacement" {
				want = "mismatch"
			}
			if got.State != want {
				t.Fatalf("%s: %+v", mode, got)
			}
		})
	}
}

func TestDiagnosticHostOriginalIdentity(t *testing.T) {
	for _, mode := range []string{"present", "exited", "old_boot", "pid_reused", "cgroup_reused", "residual", "missing_events", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := processFixture(t)
			p, err := h.observe(4321, vmUUID)
			if err != nil {
				t.Fatal(err)
			}
			r := originalFixture(t)
			r.Observed.Host = p
			group := filepath.Join(h.cgroup, "lxd/runner-one")
			testWrite(t, filepath.Join(group, "cgroup.events"), []byte("populated 0\nfrozen 0\n"), 0444)
			switch mode {
			case "exited":
				os.RemoveAll(filepath.Join(h.proc, "4321"))
			case "old_boot":
				testWrite(t, filepath.Join(h.proc, "sys/kernel/random/boot_id"), []byte(vmUUID), 0444)
			case "pid_reused":
				path := filepath.Join(h.proc, "4321/stat")
				raw, _ := os.ReadFile(path)
				testWrite(t, path, bytes.Replace(raw, []byte("876543"), []byte("876544"), 1), 0444)
			case "cgroup_reused":
				os.Rename(group, group+"-old")
				os.Mkdir(group, 0700)
			case "residual":
				testWrite(t, filepath.Join(group, "cgroup.events"), []byte("populated 1\n"), 0444)
			case "missing_events":
				os.Remove(filepath.Join(group, "cgroup.events"))
			case "symlink":
				os.Rename(group, group+"-old")
				os.Symlink(group+"-old", group)
			}
			got := h.diagnose(context.Background(), r)
			wantProcess, wantGroup := "present", "empty_observed"
			switch mode {
			case "exited":
				wantProcess = "absent_verified"
			case "old_boot":
				wantProcess = "unknown"
				wantGroup = "unknown"
			case "pid_reused":
				wantProcess = "mismatch"
			case "cgroup_reused":
				wantProcess = "unknown"
				wantGroup = "mismatch"
			case "residual":
				wantGroup = "residual"
			case "missing_events":
				wantGroup = "unknown"
			case "symlink":
				wantProcess = "unknown"
				wantGroup = "unknown"
			}
			if got.Process.State != wantProcess || got.Cgroup.State != wantGroup {
				t.Fatalf("%s: %+v", mode, got)
			}
		})
	}
}

func TestDiagnosticSharedOriginalLoaderAndImmutableReadback(t *testing.T) {
	c, path, operations := captureFixture(t)
	if _, err := c.execute(context.Background(), path, operations); err != nil {
		t.Fatal(err)
	}
	r, _ := parseRequest([]byte(requestJSON))
	dir := filepath.Join(operations, r.Operation)
	before := map[string]string{}
	for _, name := range []string{"request.json", "execution.json", "complete.json"} {
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		before[name] = string(raw)
	}
	for i := 0; i < 2; i++ {
		var record receipt
		ok, err := c.files.originalRecord(operations, r, []byte(requestJSON), strings.Repeat("6", 64), &record)
		if !ok || err != nil || record.ProducerSHA != strings.Repeat("6", 64) {
			t.Fatal("cross-binary original unavailable")
		}
	}
	for name, raw := range before {
		current, _ := os.ReadFile(filepath.Join(dir, name))
		if string(current) != raw {
			t.Fatal("diagnostic rewrote original")
		}
	}
	var record receipt
	if _, err := c.files.originalRecord(operations, r, []byte(requestJSON), strings.Repeat("7", 64), &record); err == nil {
		t.Fatal("wrong capture producer accepted")
	}
	testWrite(t, filepath.Join(dir, "complete.json"), []byte(`{"version":1`), 0400)
	if _, err := c.files.originalRecord(operations, r, []byte(requestJSON), strings.Repeat("6", 64), &record); err == nil {
		t.Fatal("partial seal accepted")
	}
}

func TestDiagnosticIndependentDimensionsAndCurrentBinding(t *testing.T) {
	r := originalFixture(t)
	b := r.Observed.Guard
	current := provider.GuardObservation{PolicySHA: b.PolicySHA, Controller: b.Controller, Repository: b.Repository, ScaleSet: b.ScaleSet, Pool: b.Pool, Image: b.Image, StateSHA: strings.Repeat("9", 64)}
	calls := 0
	d := diagnostician{job: func(context.Context, receipt) jobObservation {
		return jobObservation{State: "terminal", Conclusion: "failure"}
	}, registration: func(context.Context, receipt) resourceObservation { return unknown("fixture") }, garm: func(context.Context, receipt) garmObservation {
		return garmObservation{resourceObservation: resource("absent_verified", 0)}
	}, machine: func(context.Context, receipt) machineObservation {
		return machineObservation{resource("absent_verified", 0), resource("residual", 1), unknown("fixture")}
	}, host: func(context.Context, receipt) hostObservation { return hostObservation{} }, guard: func(context.Context) (provider.GuardObservation, error) { return current, nil }, services: func(context.Context) []serviceObservation {
		calls++
		return []serviceObservation{{Unit: "nucleiron.service", State: "observed", Invocation: "original", MainPID: 42, Configuration: "current_process_config_path_verified"}}
	}, database: func(context.Context, receipt) databaseObservation {
		return databaseObservation{resourceObservation: resource("observed", 0), Configuration: "current_file_fixed_database_verified"}
	}}
	out := d.observe(context.Background(), r, strings.Repeat("7", 64))
	if out.Closure != "unknown" || len(out.ProvenanceGaps) != 4 || out.Job.Conclusion != "failure" || out.Machine.Volumes.State != "residual" || out.Guard.State != "observed" || out.CaptureProducer == out.DiagnosticProducer || out.Database.Configuration != "current_file_and_process_config_path_verified" || out.Database.RuntimeConnection != "runtime_database_connection_unknown" || calls != 2 {
		t.Fatalf("inaccurate report: %+v", out)
	}
	current.PolicySHA = strings.Repeat("8", 64)
	d.services = func(context.Context) []serviceObservation {
		calls++
		return []serviceObservation{{Unit: "nucleiron.service", State: "observed", Invocation: fmt.Sprint(calls), Configuration: "current_process_config_path_verified"}}
	}
	out = d.observe(context.Background(), r, strings.Repeat("7", 64))
	if out.Guard.State != "mismatch" || out.Database.Configuration != "current_file_fixed_database_verified" {
		t.Fatal("changed current identities accepted")
	}
}

func TestDiagnosticCLIAdmissionAndRedaction(t *testing.T) {
	for _, args := range [][]string{{"diagnose-execution"}, {"diagnose-execution", "--request=fixture-sensitive", "--api-url=fixture-sensitive"}, {"diagnose-execution", "--capture-sha256=fixture-sensitive"}} {
		var out, diagnostic bytes.Buffer
		if Run(context.Background(), args, &out, &diagnostic) == 0 || out.Len() != 0 || diagnostic.String() != "{\"event\":\"execution_diagnostic_refused\",\"stage\":\"arguments\"}\n" {
			t.Fatalf("unredacted CLI: %q", diagnostic.String())
		}
	}
	if os.Getuid() != 0 {
		var out, diagnostic bytes.Buffer
		args := []string{"diagnose-execution", "--request=/absent", "--operations=/absent", "--capture-sha256=" + strings.Repeat("6", 64), "--github-token-file=/absent", "--garm-token-file=/absent"}
		if Run(context.Background(), args, &out, &diagnostic) == 0 || !strings.Contains(diagnostic.String(), `"stage":"identity"`) {
			t.Fatal("diagnostic accepted non-root")
		}
	}
}
