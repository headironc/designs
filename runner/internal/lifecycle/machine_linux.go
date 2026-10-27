package lifecycle

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"time"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
	"github.com/headironc/designs/runner/internal/provider"
)

type machine struct {
	PID            int64  `json:"qemu_pid"`
	VolumeName     string `json:"volume_name"`
	VolumeUUID     string `json:"volume_uuid"`
	VolumeType     string `json:"volume_type"`
	VolumePool     string `json:"volume_pool"`
	VolumeProject  string `json:"volume_project"`
	VolumeContent  string `json:"volume_content_type"`
	VolumeLocation string `json:"volume_location"`
	VolumeUsedBy   string `json:"volume_used_by"`
}

type lxdTransport struct{ transport *http.Transport }

func (t *lxdTransport) Transport() *http.Transport { return t.transport }
func (t *lxdTransport) CloseIdleConnections()      { t.transport.CloseIdleConnections() }

func (t *lxdTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The SDK uses http://unix.socket; the dialer below ignores URL endpoints.
	response, err := (readTransport{t.transport, "http", "unix.socket"}).RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// Inventories must not become empty evidence when metadata is absent/null.
	var body struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	raw, err := readResponse(response)
	if err != nil || json.Unmarshal(raw, &body) != nil || len(body.Metadata) == 0 || string(body.Metadata) == "null" {
		response.Body.Close()
		return nil, errRefused
	}
	return response, nil
}

func observeMachine(ctx context.Context, b provider.ExecutionBinding) (machine, error) {
	return lxdMachine(ctx, "/var/snap/lxd/common/lxd/unix.socket", b)
}

func lxdMachine(ctx context.Context, socket string, b provider.ExecutionBinding) (machine, error) {
	client, err := lxd.ConnectLXDUnixWithContext(ctx, socket, &lxd.ConnectionArgs{
		HTTPClient: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused }},
		TransportWrapper: func(t *http.Transport) lxd.HTTPTransporter {
			t.Proxy = nil
			t.DisableKeepAlives = true
			t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}
			return &lxdTransport{t}
		},
	})
	if err != nil {
		return machine{}, err
	}
	defer client.Disconnect()
	httpClient, err := client.GetHTTPClient()
	if err != nil {
		return machine{}, err
	}
	defer httpClient.CloseIdleConnections()
	if !client.HasExtension("virtual-machines") {
		return machine{}, errRefused
	}
	return readMachine(client.UseProject("halo-ci"), b)
}

func readMachine(client lxd.InstanceServer, b provider.ExecutionBinding) (machine, error) {
	vm, _, err := client.GetInstance(b.Name)
	if err != nil || vm == nil || vm.Name != b.Name || vm.Type != "virtual-machine" || len(vm.Profiles) != 1 || vm.Profiles[0] != "halo-ci" || vm.Config["volatile.uuid"] != b.UUID || vm.Config["volatile.base_image"] != b.Image || vm.Config["user.runner-controller-id"] != b.Controller || vm.Config["user.runner-pool-id"] != b.Pool {
		return machine{}, errRefused
	}
	for _, key := range []string{"raw.qemu", "raw.qemu.conf", "raw.qemu.conf.path"} {
		if vm.ExpandedConfig[key] != "" || vm.Config[key] != "" {
			return machine{}, errRefused
		}
	}
	// All expanded attachments are observed. Only the dedicated root disk and
	// managed bridge/none devices are supported; host paths and passthrough are refused.
	root := false
	for _, d := range vm.ExpandedDevices {
		switch d["type"] {
		case "none":
		case "nic":
			// ExpandedDevices preserves the profile's network reference. LXD
			// derives nictype from that network, so absence alone proves nothing.
			// The controlled producer uses network=nucleiron0 without nictype
			// or parent; those properties conflict with managed network binding.
			if d["network"] != "nucleiron0" || d["nictype"] != "" || d["parent"] != "" {
				return machine{}, errRefused
			}
			network, _, err := client.GetNetwork("nucleiron0")
			if err != nil || network == nil || network.Name != "nucleiron0" || network.Type != "bridge" || !network.Managed || network.Status != api.NetworkStatusCreated {
				return machine{}, errRefused
			}
		case "disk":
			if root || d["path"] != "/" || d["pool"] != "halo-ci" || d["source"] != "" || d["readonly"] != "" && d["readonly"] != "false" {
				return machine{}, errRefused
			}
			for _, key := range []string{"source.snapshot", "source.type", "source.project", "raw.mount.options", "shift", "recursive"} {
				if d[key] != "" {
					return machine{}, errRefused
				}
			}
			root = true
		default:
			return machine{}, errRefused
		}
	}
	if !root {
		return machine{}, errRefused
	}
	state, _, err := client.GetInstanceState(b.Name)
	if err != nil || state == nil || state.Pid <= 0 || state.Pid > 2147483647 || state.StatusCode != api.Running || state.Status != "Running" {
		return machine{}, errRefused
	}
	operations, err := client.GetOperations()
	if err != nil {
		return machine{}, err
	}
	for _, op := range operations {
		if !uuidPattern.MatchString(op.ID) || op.StatusCode != api.Success && op.StatusCode != api.Failure && op.StatusCode != api.Cancelled {
			return machine{}, errRefused
		}
	}
	snapshots, err := client.GetInstanceSnapshotNames(b.Name)
	if err != nil || snapshots == nil || len(snapshots) != 0 {
		return machine{}, errRefused
	}
	volume, _, err := client.GetStoragePoolVolume("halo-ci", "virtual-machine", b.Name)
	if err != nil || volume == nil || volume.Name != b.Name || volume.Type != "virtual-machine" || volume.Pool != "halo-ci" || volume.Project != "halo-ci" || volume.ContentType != "block" || !uuidPattern.MatchString(volume.Config["volatile.uuid"]) || volume.Config["volatile.uuid"] == b.UUID || len(volume.UsedBy) != 1 {
		return machine{}, errRefused
	}
	used, err := url.Parse(volume.UsedBy[0])
	if err != nil || used.Scheme != "" || used.Host != "" || used.Path != "/1.0/instances/"+b.Name || used.RawQuery != "project=halo-ci" || used.Fragment != "" {
		return machine{}, errRefused
	}
	volumeSnapshots, err := client.GetStoragePoolVolumeSnapshotNames("halo-ci", "virtual-machine", b.Name)
	if err != nil || volumeSnapshots == nil || len(volumeSnapshots) != 0 {
		return machine{}, errRefused
	}
	// Dedicated pool inventory also catches unreported writable leftovers.
	volumes, err := client.GetStoragePoolVolumes("halo-ci")
	if err != nil || volumes == nil {
		return machine{}, errRefused
	}
	found := 0
	for _, v := range volumes {
		if v.Type == "image" {
			continue
		}
		if v.Name != volume.Name || v.Type != volume.Type || v.Project != volume.Project || v.Pool != volume.Pool || v.Config["volatile.uuid"] != volume.Config["volatile.uuid"] {
			return machine{}, errRefused
		}
		found++
	}
	if found != 1 {
		return machine{}, errRefused
	}
	return machine{state.Pid, volume.Name, volume.Config["volatile.uuid"], volume.Type, volume.Pool, volume.Project, volume.ContentType, volume.Location, volume.UsedBy[0]}, nil
}
