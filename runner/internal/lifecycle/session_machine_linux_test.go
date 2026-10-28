package lifecycle

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSessionMachineRequiresGlobalResourcesAndOriginalOperations(t *testing.T) {
	for _, kind := range []string{"empty", "instance", "volume", "pending", "unknown_status", "duplicate_operation", "null_operations", "null_instances", "missing_extension", "changed_operations"} {
		t.Run(kind, func(t *testing.T) {
			b := inventoryBodies(t)
			b["/1.0"] = strings.Replace(b["/1.0"], `"network"`, `"network","operations_get_query_all_projects"`, 1)
			if kind != "instance" {
				b["/1.0/instances"] = "[]"
			}
			if kind != "volume" {
				b["/1.0/storage-pools/halo-ci/volumes"] = "[]"
			}
			switch kind {
			case "pending":
				b["/1.0/operations"] = `{"running":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status_code":103}]}`
			case "unknown_status":
				b["/1.0/operations"] = `{"success":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status_code":0}]}`
			case "duplicate_operation":
				b["/1.0/operations"] = `{"success":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status_code":200},{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status_code":200}]}`
			case "null_operations":
				b["/1.0/operations"] = `null`
			case "null_instances":
				b["/1.0/instances"] = `null`
			case "missing_extension":
				b["/1.0"] = strings.Replace(b["/1.0"], `,"operations_get_query_all_projects"`, "", 1)
			}
			var calls atomic.Int32
			socket := machineResponder(t, func(r *http.Request) (string, bool) {
				body, ok := b[r.URL.Path]
				if r.URL.Path == "/1.0/operations" {
					if r.URL.Query().Get("all-projects") != "true" {
						t.Error("session inventory used only one project's operations")
					}
					if kind == "changed_operations" && calls.Add(1) > 1 {
						body = `{"success":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status_code":200}]}`
					}
				}
				return body, ok
			})
			identity, err := observeSessionMachine(context.Background(), socket, "bbbbbbbb-0000-4000-8000-000000000002")
			if kind == "empty" {
				if err != nil || !digestPattern.MatchString(identity) {
					t.Fatalf("empty private LXD inventory rejected: %v", err)
				}
			} else if err == nil || identity != "" {
				t.Fatal("incomplete or residual inventory admitted")
			}
		})
	}
}
