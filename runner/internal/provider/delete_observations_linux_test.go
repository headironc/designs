package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lxd "github.com/canonical/lxd/client"
)

func TestDeleteLXDTargetAndVacancy(t *testing.T) {
	for _, mode := range []string{"ready", "receipt", "missing-receipt", "bad-receipt", "missing-target", "name", "uuid", "controller", "pool", "image", "profile", "container", "live", "unknown", "retained-complete", "remaining-vm", "remaining-volume", "custom-volume", "null-inventory", "null-volumes"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "delete-lxd-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			p := guardPolicy{Controller: controllerID, Pool: poolID, Image: strings.Repeat("a", 64)}
			ready, agent := true, int64(42)
			active := activeInstance{Name: "runner-one", UUID: controllerID, Ready: &ready, AgentID: &agent}
			if mode == "receipt" || mode == "missing-receipt" || mode == "bad-receipt" {
				ready = false
				active.AgentID = nil
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("LXD mutation")
					http.Error(w, "bad", 400)
					return
				}
				if r.URL.Path == "/1.0" {
					fmt.Fprint(w, `{"type":"sync","status_code":200,"metadata":{"api_extensions":["virtual-machines","storage"]}}`)
					return
				}
				if r.URL.Query().Get("project") != "halo-ci" {
					t.Error("wrong project")
					http.Error(w, "bad", 400)
					return
				}
				var metadata any
				switch r.URL.Path {
				case "/1.0/instances/runner-one":
					if mode == "missing-target" {
						http.Error(w, "missing", 404)
						return
					}
					name, kind, profiles := active.Name, "virtual-machine", []string{"halo-ci"}
					config := map[string]string{"volatile.uuid": active.UUID, "volatile.base_image": p.Image, "user.runner-controller-id": p.Controller, "user.runner-pool-id": p.Pool}
					switch mode {
					case "name":
						name = "replacement"
					case "uuid":
						config["volatile.uuid"] = poolID
					case "controller":
						config["user.runner-controller-id"] = poolID
					case "pool":
						config["user.runner-pool-id"] = controllerID
					case "image":
						config["volatile.base_image"] = strings.Repeat("b", 64)
					case "profile":
						profiles = []string{"other"}
					case "container":
						kind = "container"
					}
					metadata = map[string]any{"name": name, "type": kind, "profiles": profiles, "config": config}
				case "/1.0/instances/runner-one/files":
					if ready {
						t.Error("durable ready unexpectedly reread")
					}
					if r.URL.Query().Get("path") != "/home/runner/actions-runner/.nucleiron-bootstrap-ready" {
						t.Error("wrong receipt")
					}
					if mode == "missing-receipt" {
						http.Error(w, "missing", 404)
						return
					}
					w.Header().Set("X-LXD-type", "file")
					if mode == "bad-receipt" {
						fmt.Fprint(w, `{"instance":"other","agent_id":42}`)
					} else {
						fmt.Fprint(w, `{"instance":"runner-one","agent_id":42}`)
					}
					return
				case "/1.0/instances":
					metadata = []any{}
					if mode == "remaining-vm" {
						metadata = []any{map[string]string{"name": "runner-one", "type": "virtual-machine"}}
					}
					if mode == "null-inventory" {
						metadata = nil
					}
				case "/1.0/storage-pools/halo-ci/volumes":
					metadata = []any{map[string]string{"type": "image", "name": p.Image}}
					if mode == "remaining-volume" {
						metadata = []any{map[string]string{"type": "virtual-machine", "name": "runner-one"}}
					}
					if mode == "custom-volume" {
						metadata = []any{map[string]string{"type": "custom", "name": "leftover"}}
					}
					if mode == "null-volumes" {
						metadata = nil
					}
				case "/1.0/operations":
					metadata = map[string]any{}
					if mode == "live" || mode == "unknown" || mode == "retained-complete" {
						code := 103
						if mode == "unknown" {
							code = 999
						}
						if mode == "retained-complete" {
							code = 200
						}
						metadata = map[string]any{"operations": []any{map[string]any{"id": controllerID, "status_code": code}}}
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.Error(w, "bad", 400)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status_code": 200, "metadata": metadata})
			})}
			go server.Serve(listener)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var got *int64
			before := withLXD(ctx, socket, func(client lxd.InstanceServer) error {
				var err error
				got, err = deletionTarget(client, p, active)
				return err
			})
			beforeOK := mode == "ready" || mode == "receipt" || mode == "retained-complete" || strings.HasPrefix(mode, "remaining-") || mode == "custom-volume" || strings.HasPrefix(mode, "null-")
			if beforeOK && (before != nil || got == nil || *got != 42) {
				t.Fatalf("target accepted=%v agent=%v error=%v", beforeOK, got, before)
			}
			if !beforeOK && before == nil && got != nil {
				t.Fatalf("rejected target returned agent %d", *got)
			}
			after := withLXD(ctx, socket, vacantProject)
			afterOK := mode != "live" && mode != "unknown" && !strings.HasPrefix(mode, "remaining-") && mode != "custom-volume" && !strings.HasPrefix(mode, "null-")
			if (after == nil) != afterOK {
				t.Fatalf("vacancy expected=%v error=%v", afterOK, after)
			}
		})
	}
}
