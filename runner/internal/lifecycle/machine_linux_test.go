package lifecycle

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func machineBodies() map[string]string {
	volume := `{"name":"runner-one","type":"virtual-machine","pool":"halo-ci","project":"halo-ci","content_type":"block","config":{"volatile.uuid":"` + volumeUUID + `"},"used_by":["/1.0/instances/runner-one?project=halo-ci"]}`
	return map[string]string{
		"/1.0":                                `{"api_version":"1.0","api_status":"stable","auth":"trusted","api_extensions":["virtual-machines","instances","storage","storage_api_volume_snapshots"]}`,
		"/1.0/instances/runner-one":           `{"name":"runner-one","type":"virtual-machine","profiles":["halo-ci"],"config":{"volatile.uuid":"` + vmUUID + `","volatile.base_image":"` + strings.Repeat("1", 64) + `","user.runner-controller-id":"bbbbbbbb-0000-4000-8000-000000000002","user.runner-pool-id":"build-cccccccc-0000-4000-8000-000000000003"},"expanded_devices":{"root":{"type":"disk","path":"/","pool":"halo-ci"},"eth0":{"type":"nic"}}}`,
		"/1.0/instances/runner-one/state":     `{"status":"Running","status_code":103,"pid":4321}`,
		"/1.0/instances/runner-one/snapshots": `[]`,
		"/1.0/operations":                     `{}`,
		"/1.0/storage-pools/halo-ci/volumes/virtual-machine/runner-one":           volume,
		"/1.0/storage-pools/halo-ci/volumes/virtual-machine/runner-one/snapshots": `[]`,
		"/1.0/storage-pools/halo-ci/volumes":                                      `[` + volume + `]`,
	}
}

func machineFixture(t *testing.T, bodies map[string]string) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "lxd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("LXD write attempted")
			http.Error(w, "write", 500)
			return
		}
		if r.URL.Path != "/1.0" && r.URL.Query().Get("project") != "halo-ci" {
			t.Errorf("missing project binding on %s", r.URL)
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			t.Errorf("unexpected LXD path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"sync","status":"Success","status_code":200,"metadata":%s}`, body)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return socket
}

func TestLXDActualSDKVolumeIdentity(t *testing.T) {
	socket := machineFixture(t, machineBodies())
	vm, err := lxdMachine(context.Background(), socket, binding())
	if err != nil {
		t.Fatal(err)
	}
	if vm.PID != 4321 || vm.VolumeUUID != volumeUUID || vm.VolumeUUID == vmUUID || vm.VolumeUsedBy != "/1.0/instances/runner-one?project=halo-ci" {
		t.Fatalf("wrong machine identity: %+v", vm)
	}
}

func TestLXDRefusesReplacementAndUnknownTopology(t *testing.T) {
	instance := "/1.0/instances/runner-one"
	volume := "/1.0/storage-pools/halo-ci/volumes/virtual-machine/runner-one"
	for _, tc := range []struct{ name, path, old, new string }{
		{"same-name replacement", instance, vmUUID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
		{"controller", instance, "bbbbbbbb-0000-4000-8000-000000000002", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
		{"pool", instance, "build-cccccccc-0000-4000-8000-000000000003", "bad"},
		{"image", instance, strings.Repeat("1", 64), strings.Repeat("2", 64)},
		{"profile", instance, `"profiles":["halo-ci"]`, `"profiles":["default"]`},
		{"host disk", instance, `"pool":"halo-ci"`, `"pool":"halo-ci","source":"/host/path"`},
		{"extra disk", instance, `"eth0":{"type":"nic"}`, `"extra":{"type":"disk","path":"/extra","source":"/tmp"}`},
		{"raw qemu", instance, `"expanded_devices":`, `"expanded_config":{"raw.qemu":"-drive file=/tmp/escape"},"expanded_devices":`},
		{"not running", "/1.0/instances/runner-one/state", `"status_code":103`, `"status_code":102`},
		{"pid null", "/1.0/instances/runner-one/state", `"pid":4321`, `"pid":null`},
		{"pid overflow", "/1.0/instances/runner-one/state", `"pid":4321`, `"pid":2147483648`},
		{"missing volume uuid", volume, `"volatile.uuid":"` + volumeUUID + `"`, `"other":"missing"`},
		{"wrong volume uuid", volume, volumeUUID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},
		{"vm uuid is not volume uuid", volume, volumeUUID, vmUUID},
		{"wrong volume project", volume, `"project":"halo-ci"`, `"project":"default"`},
		{"wrong attachment", volume, `runner-one?project=halo-ci`, `other?project=halo-ci`},
		{"volume snapshot", "/1.0/storage-pools/halo-ci/volumes/virtual-machine/runner-one/snapshots", `[]`, `["/1.0/storage-pools/halo-ci/volumes/virtual-machine/runner-one/snapshots/snap0"]`},
		{"instance snapshot", "/1.0/instances/runner-one/snapshots", `[]`, `["/1.0/instances/runner-one/snapshots/snap0"]`},
		{"unknown operation", "/1.0/operations", `{}`, `{"running":[{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","status_code":103}]}`},
		{"null snapshots", "/1.0/instances/runner-one/snapshots", `[]`, `null`},
		{"null operations", "/1.0/operations", `{}`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := machineBodies()
			bodies[tc.path] = strings.Replace(bodies[tc.path], tc.old, tc.new, 1)
			socket := machineFixture(t, bodies)
			if _, err := lxdMachine(context.Background(), socket, binding()); err == nil {
				t.Fatal("accepted invalid LXD identity")
			}
		})
	}
}
