package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	lxd "github.com/canonical/lxd/client"
	systemd "github.com/coreos/go-systemd/v22/dbus"
	"golang.org/x/sys/unix"
)

var requiredUnits = []string{"nucleiron-logs.service", "nucleiron-gateway.service", "nucleiron-firewall.service", "nucleiron-dns.service", "nucleiron-egress.service"}

func hostCapacity(ctx context.Context) error {
	for _, item := range []struct {
		path    string
		reserve uint64
	}{{"/var/snap/lxd/common/halostorage", 116 << 30}, {"/var/log/nucleiron/guests", 256 << 20}} {
		if err := mountReserve(item.path, item.reserve); err != nil {
			return err
		}
	}
	connect, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	bus, err := systemd.NewSystemConnectionContext(connect)
	if err != nil {
		return err
	}
	defer bus.Close()
	return activeUnits(connect, bus)
}

func activeUnits(ctx context.Context, bus *systemd.Conn) error {
	for _, unit := range requiredUnits {
		value, err := bus.GetUnitPropertyContext(ctx, unit, "ActiveState")
		if err != nil {
			return err
		}
		if value == nil || value.Value.Value() != "active" {
			return fault{"Required service unavailable", false}
		}
	}
	return nil
}

func mountReserve(path string, reserve uint64) error {
	f, err := openPath(path, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return fault{"Required storage mount absent", false}
	}
	defer f.Close()
	parent, err := openPath(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer parent.Close()
	var here, above unix.Statx_t
	for _, item := range []struct {
		fd   int
		stat *unix.Statx_t
	}{{int(f.Fd()), &here}, {int(parent.Fd()), &above}} {
		if err := unix.Statx(item.fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, item.stat); err != nil {
			return err
		}
		if item.stat.Mask&unix.STATX_MNT_ID == 0 {
			return errRefused
		}
	}
	if here.Mnt_id == above.Mnt_id {
		return fault{"Required storage mount absent", false}
	}
	var disk unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &disk); err != nil {
		return err
	}
	if disk.Bsize <= 0 || disk.Bavail < (reserve+uint64(disk.Bsize)-1)/uint64(disk.Bsize) {
		return fault{"Storage reserve exhausted", false}
	}
	if !samePath(f, path) {
		return errRefused
	}
	return nil
}

var errNotReady = errors.New("ready receipt unavailable")

// Bound the SDK's actual transport, including server discovery and file bodies.
// All operations are GETs over the explicit Unix socket; no proxy or redirect.
type lxdReads struct{ transport *http.Transport }

func (r *lxdReads) Transport() *http.Transport { return r.transport }

func (r *lxdReads) CloseIdleConnections() { r.transport.CloseIdleConnections() }
func (r *lxdReads) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, errRefused
	}
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	response, err := r.transport.RoundTrip(req.Clone(ctx))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	file := strings.HasSuffix(req.URL.Path, "/files")
	if response.StatusCode != http.StatusOK {
		if file {
			return nil, errNotReady
		}
		return nil, errRefused
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 2*1024*1024 {
		if file {
			return nil, errNotReady
		}
		return nil, errRefused
	}
	if file && response.Header.Get("X-LXD-type") != "file" {
		return nil, fault{"Bootstrap completion record invalid", true}
	}
	if !validInventory(req.URL.Path, raw) {
		return nil, errRefused
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

func observeReady(ctx context.Context, policy guardPolicy, active activeInstance) (*int64, error) {
	return lxdReady(ctx, "/var/snap/lxd/common/lxd/unix.socket", policy, active)
}

func lxdReady(ctx context.Context, socket string, policy guardPolicy, active activeInstance) (*int64, error) {
	var agent *int64
	err := withLXD(ctx, socket, func(client lxd.InstanceServer) error {
		var err error
		agent, err = readReady(client, policy, active)
		return err
	})
	return agent, err
}

func withLXD(ctx context.Context, socket string, observe func(lxd.InstanceServer) error) error {
	client, err := lxd.ConnectLXDUnixWithContext(ctx, socket, &lxd.ConnectionArgs{
		HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused }},
		TransportWrapper: func(t *http.Transport) lxd.HTTPTransporter {
			t.Proxy, t.DisableKeepAlives = nil, true
			// The upstream Unix dialer ignores context; bind cancellation and the
			// fixed socket here instead of inheriting environment-derived paths.
			t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}
			return &lxdReads{transport: t}
		},
	})
	if err != nil {
		return err
	}
	defer client.Disconnect()
	httpClient, err := client.GetHTTPClient()
	if err != nil {
		return err
	}
	defer httpClient.CloseIdleConnections()
	if !client.HasExtension("virtual-machines") {
		return errRefused
	}
	return observe(client.UseProject("halo-ci"))
}

func readReady(client lxd.InstanceServer, policy guardPolicy, active activeInstance) (*int64, error) {
	item, _, err := client.GetInstance(active.Name)
	if err != nil {
		return nil, err
	}
	if item == nil || item.Config["user.runner-controller-id"] != policy.Controller || item.Config["user.runner-pool-id"] != policy.Pool || item.Config["volatile.base_image"] != policy.Image || len(item.Profiles) != 1 || item.Profiles[0] != "halo-ci" || item.Type != "virtual-machine" {
		return nil, fault{"Instance ownership or image differs", true}
	}
	if item.Name != active.Name || item.Config["volatile.uuid"] != active.UUID {
		return nil, fault{"Instance identity differs from durable record", true}
	}
	body, info, err := client.GetInstanceFile(active.Name, "/home/runner/actions-runner/.nucleiron-bootstrap-ready")
	if errors.Is(err, errNotReady) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errRefused
	}
	defer body.Close()
	if info == nil || info.Type != "file" {
		return nil, fault{"Bootstrap completion record invalid", true}
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, nil
	}
	var ready struct {
		Instance string `json:"instance"`
		AgentID  int64  `json:"agent_id"`
	}
	if decodeObject(raw, &ready, true) != nil || ready.Instance != active.Name || ready.AgentID <= 0 {
		return nil, fault{"Bootstrap completion record invalid", true}
	}
	return &ready.AgentID, nil
}
