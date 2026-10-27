package provider

import (
	"bytes"
	"context"
	"encoding/json"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
)

const lxdSocket = "/var/snap/lxd/common/lxd/unix.socket"

func beforeCreate(ctx context.Context, p createPolicy) error {
	return withLXD(ctx, lxdSocket, func(client lxd.InstanceServer) error { return createVacancy(client, p) })
}

func createVacancy(client lxd.InstanceServer, p createPolicy) error {
	// Containers and other leftovers also occupy this dedicated project.
	all, err := client.GetInstances(lxd.GetInstancesArgs{})
	if err != nil || len(all) != 0 {
		return errRefused
	}
	volumes, err := client.GetStoragePoolVolumes("halo-ci")
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		if volume.Type != "image" {
			return errRefused
		}
	}
	if err := noLiveOperations(client); err != nil {
		return err
	}
	alias, _, err := client.GetImageAlias(p.Alias)
	if err != nil || alias == nil || alias.Target != p.Image {
		return errRefused
	}
	return nil
}

// SDK zero values must not turn absent inventory into evidence of vacancy.
func validInventory(path string, raw []byte) bool {
	if path != "/1.0/instances" && path != "/1.0/containers" && path != "/1.0/storage-pools/halo-ci/volumes" && path != "/1.0/operations" {
		return true
	}
	var envelope struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if decodeObject(raw, &envelope, true) != nil {
		return false
	}
	metadata := bytes.TrimSpace(envelope.Metadata)
	if len(metadata) == 0 {
		return false
	}
	if path != "/1.0/operations" {
		return metadata[0] == '['
	}
	var groups map[string]json.RawMessage
	if decodeObject(metadata, &groups, true) != nil {
		return false
	}
	for _, group := range groups {
		group = bytes.TrimSpace(group)
		if len(group) == 0 || group[0] != '[' {
			return false
		}
	}
	return true
}

func afterCreate(ctx context.Context, p createPolicy, name string) (string, error) {
	var id string
	err := withLXD(ctx, lxdSocket, func(client lxd.InstanceServer) error {
		var err error
		id, err = createdInstance(client, p, name)
		return err
	})
	return id, err
}

func createdInstance(client lxd.InstanceServer, p createPolicy, name string) (string, error) {
	item, _, err := client.GetInstance(name)
	if err != nil || item == nil || item.Name != name || item.Type != "virtual-machine" || len(item.Profiles) != 1 || item.Profiles[0] != "halo-ci" || item.Config["user.runner-controller-id"] != p.Controller || item.Config["user.runner-pool-id"] != p.Pool || item.Config["volatile.base_image"] != p.Image || !uuid.MatchString(item.Config["volatile.uuid"]) {
		return "", errRefused
	}
	if err := noLiveOperations(client); err != nil {
		return "", err
	}
	return item.Config["volatile.uuid"], nil
}

func noLiveOperations(client lxd.InstanceServer) error {
	operations, err := client.GetOperations()
	if err != nil {
		return err
	}
	// LXD retains completed operations briefly. Their presence does not mean
	// work is still running; unknown/missing status never counts as terminal.
	for _, op := range operations {
		if !uuid.MatchString(op.ID) || (op.StatusCode != api.Success && op.StatusCode != api.Failure && op.StatusCode != api.Cancelled) {
			return errRefused
		}
	}
	return nil
}
