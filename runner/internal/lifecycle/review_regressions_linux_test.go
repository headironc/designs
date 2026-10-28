package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cloudbase/garm/params"
)

func TestDiagnosticGARMRequiredWireEvidence(t *testing.T) {
	original := originalFixture(t)
	for path, fields := range map[string][]string{
		"/api/v1/controller-info":                                   {"controller_id"},
		"/api/v1/repositories/cccccccc-0000-4000-8000-000000000003": {"id", "owner", "name", "endpoint", "endpoint.endpoint_type", "endpoint.base_url", "endpoint.api_base_url"},
		"/api/v1/scalesets/27":                                      {"id", "scale_set_id", "name", "provider_name", "repo_id", "endpoint", "endpoint.endpoint_type", "endpoint.base_url", "endpoint.api_base_url"},
	} {
		for _, field := range fields {
			for _, mode := range []string{"missing", "null"} {
				t.Run(path+"/"+field+"/"+mode, func(t *testing.T) {
					b := diagnosticBodies()
					var body map[string]json.RawMessage
					json.Unmarshal([]byte(b[path]), &body)
					target, key := body, field
					if len(field) > 9 && field[:9] == "endpoint." {
						target = map[string]json.RawMessage{}
						json.Unmarshal(body["endpoint"], &target)
						key = field[9:]
					}
					if mode == "missing" {
						delete(target, key)
					} else {
						target[key] = json.RawMessage("null")
					}
					if target != nil && key != field {
						raw, _ := json.Marshal(target)
						body["endpoint"] = raw
					}
					raw, _ := json.Marshal(body)
					b[path] = string(raw)
					got := platformFixture(t, b).diagnoseGARM(context.Background(), original)
					if got.State != "unknown" {
						t.Fatalf("missing evidence became %s", got.State)
					}
				})
			}
		}
		t.Run(path+"/null", func(t *testing.T) {
			b := diagnosticBodies()
			b[path] = "null"
			if got := platformFixture(t, b).diagnoseGARM(context.Background(), original); got.State != "unknown" {
				t.Fatalf("null evidence became %+v", got)
			}
		})
	}
	for _, path := range []string{"/api/v1/controller-info", "/api/v1/repositories/cccccccc-0000-4000-8000-000000000003", "/api/v1/scalesets/27"} {
		b := diagnosticBodies()
		var row map[string]json.RawMessage
		json.Unmarshal([]byte(b[path]), &row)
		key, value := "id", json.RawMessage(`"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"`)
		if path == "/api/v1/controller-info" {
			key = "controller_id"
		}
		if path == "/api/v1/scalesets/27" {
			value = json.RawMessage("28")
		}
		row[key] = value
		raw, _ := json.Marshal(row)
		b[path] = string(raw)
		if got := platformFixture(t, b).diagnoseGARM(context.Background(), original); got.State != "mismatch" {
			t.Fatalf("wrong observed ID lost: %+v", got)
		}
	}
}

func TestDiagnosticNetworkObservationFailureIsUnknown(t *testing.T) {
	for _, mode := range []string{"malformed", "null", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			b := inventoryBodies(t)
			socket := machineResponder(t, func(q *http.Request) (string, bool) {
				if q.URL.Path == "/1.0/networks/nucleiron0" {
					if mode == "malformed" {
						return "[]", true
					}
					if mode == "invalid" {
						return `{"name":"nucleiron0","type":"physical","managed":true,"status":"Created"}`, true
					}
					return "null", true
				}
				body, ok := b[q.URL.Path]
				return body, ok
			})
			got := diagnoseMachine(context.Background(), socket, originalFixture(t))
			want := "unknown"
			if mode == "invalid" {
				want = "mismatch"
			}
			if got.Instances.State != want {
				t.Fatalf("lookup classification became %+v", got.Instances)
			}
			if _, err := lxdMachine(context.Background(), socket, binding()); err == nil {
				t.Fatal("capture accepted unavailable network")
			}
		})
	}
}

func TestDiagnosticGARMInstanceWireDefaults(t *testing.T) {
	r := originalFixture(t)
	for _, path := range []string{"/api/v1/instances", "/api/v1/scalesets/27/instances"} {
		for _, field := range []string{"id", "name", "provider_name", "provider_id", "agent_id", "scale_set_id", "status", "runner_status"} {
			for _, mode := range []string{"missing", "null"} {
				t.Run(path+"/"+field+"/"+mode, func(t *testing.T) {
					b := diagnosticBodies()
					var rows []map[string]json.RawMessage
					json.Unmarshal([]byte(b[path]), &rows)
					if mode == "missing" {
						delete(rows[0], field)
					} else {
						rows[0][field] = json.RawMessage("null")
					}
					raw, _ := json.Marshal(rows)
					b[path] = string(raw)
					got := platformFixture(t, b).diagnoseGARM(context.Background(), r)
					if got.State != "unknown" {
						t.Fatalf("incomplete identity became %+v", got)
					}
				})
			}
		}
	}
	// Use the actual omitempty producer. Pending allocation fields are valid
	// omissions, so an additional pending record remains visible as residual.
	b := diagnosticBodies()
	var exact params.Instance
	json.Unmarshal([]byte(b["/api/v1/instances/runner-one"]), &exact)
	pending := params.Instance{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "pending-one", ProviderName: "halo-lxd", ScaleSetID: 27, Status: "pending_create"}
	raw, _ := json.Marshal([]params.Instance{exact, pending})
	b["/api/v1/instances"], b["/api/v1/scalesets/27/instances"] = string(raw), string(raw)
	if got := platformFixture(t, b).diagnoseGARM(context.Background(), r); got.State != "residual" || got.Count != 2 {
		t.Fatalf("valid pending allocation refused: %+v", got)
	}
	pending.ID = r.Expected.Instance
	pending.Name = r.Observed.RunnerName
	raw, _ = json.Marshal([]params.Instance{pending})
	b["/api/v1/instances"], b["/api/v1/scalesets/27/instances"] = string(raw), string(raw)
	if got := platformFixture(t, b).diagnoseGARM(context.Background(), r); got.State != "unknown" || got.Code != "garm_instance_binding_unassigned" || got.TargetStatus != "pending_create" {
		t.Fatalf("unassigned target lost actual status: %+v", got)
	}
}

func TestDatabaseSameNameConflictPrecedesExactRow(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("deleted=%v/reverse=%v", deleted, reverse), func(t *testing.T) {
				db, files, r := databaseFixture(t, "")
				if _, err := db.Exec(`DELETE FROM instances`); err != nil {
					t.Fatal(err)
				}
				timestamp := "NULL"
				if deleted {
					timestamp = "'2026-10-28'"
				}
				exact := `INSERT INTO instances VALUES('dddddddd-0000-4000-8000-000000000004','runner-one','runner-one',9007199254740999,27,NULL,'running','active',` + timestamp + `)`
				conflict := `INSERT INTO instances VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','runner-one','runner-one',1,27,NULL,'running','active',NULL)`
				queries := []string{conflict, exact}
				if reverse {
					queries = []string{exact, conflict}
				}
				for _, query := range queries {
					if _, err := db.Exec(query); err != nil {
						t.Fatal(err)
					}
				}
				got := readDatabase(context.Background(), files, r)
				if got.State != "mismatch" || got.Target != "mismatch" || got.TargetStatus != "" || got.Count != 2 {
					t.Fatalf("deleted=%v reversed=%v: %+v", deleted, reverse, got)
				}
			})
		}
	}
}
