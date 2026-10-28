package lifecycle

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/canonical/lxd/shared/api"
)

func inventoryBodies(t *testing.T) map[string]string {
	t.Helper()
	b := machineBodies()
	b["/1.0"] = strings.Replace(b["/1.0"], `"network"`, `"network","instance_all_projects","container_full","storage_volumes_all_projects"`, 1)
	var vm api.Instance
	if err := json.Unmarshal([]byte(b["/1.0/instances/runner-one"]), &vm); err != nil {
		t.Fatal(err)
	}
	vm.Project = "halo-ci"
	// Fixed qemu.RenderFull initializes these only while appending entries.
	// The actual API marshaler emits null (not omission) when they remain nil.
	full := api.InstanceFull{Instance: vm}
	raw, err := json.Marshal([]api.InstanceFull{full})
	if err != nil {
		t.Fatal(err)
	}
	b["/1.0/instances"] = string(raw)
	return b
}

func TestDiagnosticLXDInventoriesAndProducerNullLists(t *testing.T) {
	r := originalFixture(t)
	for _, mode := range []string{"present", "absent", "snapshot", "backup", "writable_attachment", "other_project_volume", "instance_replacement", "volume_replacement", "missing_snapshots", "missing_extension", "null_inventory", "late_operation"} {
		t.Run(mode, func(t *testing.T) {
			b := inventoryBodies(t)
			switch mode {
			case "absent":
				b["/1.0/instances"] = "[]"
				b["/1.0/storage-pools/halo-ci/volumes"] = "[]"
			case "snapshot":
				b["/1.0/instances"] = strings.Replace(b["/1.0/instances"], `"snapshots":null`, `"snapshots":[{"name":"runner-one/snap0","config":{"volatile.uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}]`, 1)
			case "backup":
				b["/1.0/instances"] = strings.Replace(b["/1.0/instances"], `"backups":null`, `"backups":[{"name":"runner-one/backup0"}]`, 1)
			case "writable_attachment":
				b["/1.0/instances"] = strings.Replace(b["/1.0/instances"], `"root":`, `"extra":{"type":"disk","path":"/extra","pool":"other"},"root":`, 1)
			case "other_project_volume":
				b["/1.0/instances"] = "[]"
				b["/1.0/storage-pools/halo-ci/volumes"] = strings.ReplaceAll(strings.ReplaceAll(b["/1.0/storage-pools/halo-ci/volumes"], "halo-ci\"", "other\""), `"pool":"other"`, `"pool":"halo-ci"`)
				b["/1.0/storage-pools/halo-ci/volumes"] = strings.ReplaceAll(b["/1.0/storage-pools/halo-ci/volumes"], volumeUUID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
			case "instance_replacement":
				b["/1.0/instances"] = strings.ReplaceAll(b["/1.0/instances"], vmUUID, volumeUUID)
			case "volume_replacement":
				b["/1.0/storage-pools/halo-ci/volumes"] = strings.ReplaceAll(b["/1.0/storage-pools/halo-ci/volumes"], volumeUUID, vmUUID)
			case "missing_snapshots":
				b["/1.0/instances"] = strings.Replace(b["/1.0/instances"], `,"snapshots":null`, "", 1)
			case "missing_extension":
				b["/1.0"] = strings.Replace(b["/1.0"], `,"instance_all_projects"`, "", 1)
			case "null_inventory":
				b["/1.0/instances"] = "null"
			case "late_operation":
				b["/1.0/operations"] = `{"running":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status_code":103}]}`
			}
			got := diagnoseMachine(context.Background(), machineFixture(t, b), r)
			switch mode {
			case "present":
				if got.Instances.State != "present" || got.Volumes.State != "present" {
					t.Fatalf("%+v", got)
				}
			case "absent":
				if got.Instances.State != "absent_verified" || got.Volumes.State != "absent_verified" {
					t.Fatalf("%+v", got)
				}
			case "snapshot", "backup":
				if got.Instances.State != "residual" || got.Instances.Count != 2 {
					t.Fatalf("%+v", got)
				}
			case "other_project_volume":
				if got.Volumes.State == "absent_verified" || got.Volumes.Count != 1 {
					t.Fatalf("%+v", got)
				}
			case "instance_replacement", "writable_attachment":
				if got.Instances.State != "mismatch" {
					t.Fatalf("%+v", got)
				}
			case "volume_replacement":
				if got.Volumes.State != "mismatch" {
					t.Fatalf("%+v", got)
				}
			case "missing_snapshots", "missing_extension", "null_inventory":
				if got.Instances.State != "unknown" {
					t.Fatalf("%+v", got)
				}
			case "late_operation":
				if got.Operations.State != "pending_or_unknown" {
					t.Fatalf("%+v", got)
				}
			}
		})
	}
}

func TestDiagnosticLXDDetectsEqualCountIdentityChanges(t *testing.T) {
	r := originalFixture(t)
	for _, path := range []string{"/1.0/instances", "/1.0/storage-pools/halo-ci/volumes", "/1.0/operations"} {
		t.Run(path, func(t *testing.T) {
			b := inventoryBodies(t)
			if path == "/1.0/instances" {
				b[path] = strings.Replace(b[path], `"snapshots":null`, `"snapshots":[{"name":"runner-one/snap0","config":{"volatile.uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}]`, 1)
			}
			if path == "/1.0/storage-pools/halo-ci/volumes" {
				b[path] = strings.ReplaceAll(strings.ReplaceAll(b[path], volumeUUID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), "runner-one", "leftover")
			}
			if path == "/1.0/operations" {
				b[path] = `{"running":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status_code":103}]}`
			}
			var calls atomic.Int32
			socket := machineResponder(t, func(q *http.Request) (string, bool) {
				body, ok := b[q.URL.Path]
				if q.URL.Path == path && calls.Add(1) > 1 {
					switch path {
					case "/1.0/instances":
						body = strings.ReplaceAll(body, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
					case "/1.0/storage-pools/halo-ci/volumes":
						body = strings.ReplaceAll(body, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
					case "/1.0/operations":
						body = strings.ReplaceAll(body, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
					}
				}
				return body, ok
			})
			got := diagnoseMachine(context.Background(), socket, r)
			target := got.Instances
			if path == "/1.0/storage-pools/halo-ci/volumes" {
				target = got.Volumes
			}
			if path == "/1.0/operations" {
				target = got.Operations
			}
			if target.State != "unknown" || !strings.HasSuffix(target.Code, "_changed") {
				t.Fatalf("equal-count change collapsed: %+v", target)
			}
		})
	}
}

func TestDiagnosticLXDDetectsBackupRename(t *testing.T) {
	b := inventoryBodies(t)
	b["/1.0/instances"] = strings.Replace(b["/1.0/instances"], `"backups":null`, `"backups":[{"name":"runner-one/backup0"}]`, 1)
	var calls atomic.Int32
	socket := machineResponder(t, func(q *http.Request) (string, bool) {
		body, ok := b[q.URL.Path]
		if q.URL.Path == "/1.0/instances" && calls.Add(1) > 1 {
			body = strings.Replace(body, "backup0", "backup1", 1)
		}
		return body, ok
	})
	got := diagnoseMachine(context.Background(), socket, originalFixture(t))
	if got.Instances.State != "unknown" || got.Instances.Code != "lxd_instances_changed" {
		t.Fatalf("same count renamed backup hidden: %+v", got)
	}
}
