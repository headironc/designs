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

func TestCreateLXDObservations(t *testing.T) {
	for _, mode := range []string{"empty", "completed", "vm", "volume", "alias", "running", "unknown-status", "missing-inventory", "null-inventory", "null-operations", "null-group", "wrong-instance", "wrong-uuid"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "create-lxd-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			p := createPolicy{Controller: controllerID, Pool: poolID, Image: strings.Repeat("a", 64), Alias: "runner-image"}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("write request")
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
				case "/1.0/instances":
					metadata = []any{}
					if mode == "vm" {
						metadata = []any{map[string]string{"name": "leftover", "type": "virtual-machine"}}
					}
					if mode == "null-inventory" {
						metadata = nil
					}
					if mode == "missing-inventory" {
						fmt.Fprint(w, `{"type":"sync","status_code":200}`)
						return
					}
				case "/1.0/storage-pools/halo-ci/volumes":
					metadata = []any{map[string]string{"type": "image", "name": p.Image}}
					if mode == "volume" {
						metadata = []any{map[string]string{"type": "virtual-machine", "name": "leftover"}}
					}
				case "/1.0/operations":
					metadata = map[string]any{}
					if mode == "running" || mode == "completed" || mode == "unknown-status" {
						code := 103
						if mode == "completed" {
							code = 200
						}
						if mode == "unknown-status" {
							code = 0
						}
						metadata = map[string]any{"running": []any{map[string]any{"id": controllerID, "status_code": code}}}
					}
					if mode == "null-operations" {
						metadata = nil
					}
					if mode == "null-group" {
						metadata = map[string]any{"running": nil}
					}
				case "/1.0/images/aliases/runner-image":
					target := p.Image
					if mode == "alias" {
						target = "wrong"
					}
					metadata = map[string]string{"target": target}
				case "/1.0/instances/runner-one":
					name, id := "runner-one", controllerID
					if mode == "wrong-instance" {
						name = "other"
					}
					if mode == "wrong-uuid" {
						id = ""
					}
					metadata = map[string]any{"name": name, "type": "virtual-machine", "profiles": []string{"halo-ci"}, "config": map[string]string{"volatile.uuid": id, "volatile.base_image": p.Image, "user.runner-controller-id": p.Controller, "user.runner-pool-id": p.Pool}}
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
			before := withLXD(ctx, socket, func(client lxd.InstanceServer) error { return createVacancy(client, p) })
			beforeOK := mode == "empty" || mode == "completed" || mode == "wrong-instance" || mode == "wrong-uuid"
			if (before == nil) != beforeOK {
				t.Fatalf("preflight: %v", before)
			}
			after := withLXD(ctx, socket, func(client lxd.InstanceServer) error { _, err := createdInstance(client, p, "runner-one"); return err })
			afterOK := mode != "running" && mode != "unknown-status" && mode != "null-operations" && mode != "null-group" && mode != "wrong-instance" && mode != "wrong-uuid"
			if (after == nil) != afterOK {
				t.Fatalf("post-create: %v", after)
			}
		})
	}
}
