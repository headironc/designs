package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

func TestLXDAdapter(t *testing.T) {
	for _, tc := range []struct {
		name, receipt  string
		status         int
		mutate         func(map[string]any)
		fail, notReady bool
	}{
		{name: "ready", receipt: `{"instance":"runner-one","agent_id":9007199254740993}`},
		{name: "missing", status: 404, notReady: true},
		{name: "invalid JSON", receipt: `{`, notReady: true},
		{name: "oversized", receipt: strings.Repeat("x", 2*1024*1024+1), notReady: true},
		{name: "fraction", receipt: `{"instance":"runner-one","agent_id":1.5}`, fail: true},
		{name: "boolean", receipt: `{"instance":"runner-one","agent_id":true}`, fail: true},
		{name: "wrong receipt", receipt: `{"instance":"other","agent_id":1}`, fail: true},
		{name: "duplicate", receipt: `{"instance":"other","instance":"runner-one","agent_id":1}`, fail: true},
		{name: "wrong VM", mutate: func(m map[string]any) { m["name"] = "other" }, fail: true},
		{name: "missing config", mutate: func(m map[string]any) { delete(m, "config") }, fail: true},
		{name: "wrong image", mutate: func(m map[string]any) { m["config"].(map[string]string)["volatile.base_image"] = "wrong" }, fail: true},
		{name: "wrong UUID", mutate: func(m map[string]any) { m["config"].(map[string]string)["volatile.uuid"] = "wrong" }, fail: true},
		{name: "wrong profile", mutate: func(m map[string]any) { m["profiles"] = []string{"halo-ci", "default"} }, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A short path stays below AF_UNIX's sockaddr limit in Go's test cache.
			dir, err := os.MkdirTemp("", "guard-lxd-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("non-read request")
					http.Error(w, "bad", 400)
					return
				}
				if r.URL.Path == "/1.0" {
					fmt.Fprint(w, `{"type":"sync","status_code":200,"metadata":{"api_extensions":["virtual-machines"],"environment":{"server_version":"6.10.0"}}}`)
					return
				}
				if r.URL.Query().Get("project") != "halo-ci" {
					t.Error("wrong project")
					http.Error(w, "bad", 400)
					return
				}
				switch r.URL.Path {
				case "/1.0/instances/runner-one":
					item := map[string]any{"name": "runner-one", "type": "virtual-machine", "profiles": []string{"halo-ci"}, "config": map[string]string{"user.runner-controller-id": controllerID, "user.runner-pool-id": poolID, "volatile.uuid": controllerID, "volatile.base_image": strings.Repeat("a", 64)}}
					if tc.mutate != nil {
						tc.mutate(item)
					}
					json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status_code": 200, "metadata": item})
				case "/1.0/instances/runner-one/files":
					if r.URL.Query().Get("path") != "/home/runner/actions-runner/.nucleiron-bootstrap-ready" {
						t.Error("wrong ready path")
					}
					w.Header().Set("X-LXD-type", "file")
					w.Header().Set("X-LXD-mode", "0600")
					w.Header().Set("X-LXD-uid", "1000")
					w.Header().Set("X-LXD-gid", "1000")
					if tc.status != 0 {
						w.WriteHeader(tc.status)
					}
					fmt.Fprint(w, tc.receipt)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.Error(w, "bad", 400)
				}
			})}
			go server.Serve(listener)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			agent, err := lxdReady(ctx, socket, guardPolicy{Controller: controllerID, Repository: repositoryID, ScaleSet: "nucleiron-halo", Pool: poolID, Image: strings.Repeat("a", 64)}, activeInstance{Name: "runner-one", UUID: controllerID})
			if (err != nil) != tc.fail {
				t.Fatalf("agent=%v err=%v", agent, err)
			}
			if !tc.fail && !tc.notReady && (agent == nil || *agent != 9007199254740993) {
				t.Fatalf("wrong agent: %v", agent)
			}
			if tc.notReady && agent != nil {
				t.Fatal("not-ready became ready")
			}
		})
	}
}

func TestLXDTransportCancellationAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(strconv.FormatBool(redirect), func(t *testing.T) {
			dir, err := os.MkdirTemp("", "guard-lxd-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "socket")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				if redirect {
					http.Redirect(w, r, "http://elsewhere/", 302)
					return
				}
				<-release
			})}
			go server.Serve(listener)
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := lxdReady(ctx, socket, guardPolicy{}, activeInstance{}); done <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("no request")
			}
			if !redirect {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("accepted")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SDK ignored cancellation")
			}
		})
	}
}

type unitProperties struct{ state any }

func (p unitProperties) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	if iface != "org.freedesktop.systemd1.Unit" || name != "ActiveState" {
		return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("unexpected property"))
	}
	return dbus.MakeVariant(p.state), nil
}

func TestSystemdAdapterOnPrivateBus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dbus-daemon", "--session", "--nofork", "--print-address=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	address, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	dial := func() (*dbus.Conn, error) {
		c, err := dbus.Dial(strings.TrimSpace(address), dbus.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if err = c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err == nil {
			err = c.Hello()
		}
		if err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
	service, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.RequestName("org.freedesktop.systemd1", dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	bus, err := systemd.NewConnection(dial)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	for _, state := range []any{"active", "inactive", int32(1)} {
		for _, unit := range requiredUnits {
			path := dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + strings.NewReplacer("-", "_2d", ".", "_2e").Replace(unit))
			if err := service.Export(unitProperties{state}, path, "org.freedesktop.DBus.Properties"); err != nil {
				t.Fatal(err)
			}
		}
		err := activeUnits(ctx, bus)
		if (err == nil) != (state == "active") {
			t.Fatalf("state=%v err=%v", state, err)
		}
	}
}

func TestMountReserveRejectsOrdinaryDirectory(t *testing.T) {
	if err := mountReserve(t.TempDir(), 0); err == nil {
		t.Fatal("directory accepted as mount")
	}
	if err := mountReserve("/proc", 0); err != nil {
		t.Fatalf("proc mount observation: %v", err)
	}
	if err := mountReserve("/proc", 1); err == nil {
		t.Fatal("reserve unavailable but accepted")
	}
}
