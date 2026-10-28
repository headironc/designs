package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
)

func diagnoseMachine(ctx context.Context, socket string, r receipt) machineObservation {
	unknownAll := func() machineObservation {
		return machineObservation{unknown("lxd_inventory_unavailable"), unknown("lxd_volume_inventory_unavailable"), unknown("lxd_operations_unavailable")}
	}
	var out machineObservation
	err := withMachineClient(ctx, socket, func(client lxd.InstanceServer) error {
		out = machineObservation{instanceInventory(client, r), volumeInventory(client, r), operationInventory(client)}
		again := machineObservation{instanceInventory(client, r), volumeInventory(client, r), operationInventory(client)}
		if out.Instances != again.Instances {
			out.Instances = unknown("lxd_instances_changed")
		}
		if out.Volumes != again.Volumes {
			out.Volumes = unknown("lxd_volumes_changed")
		}
		if out.Operations != again.Operations {
			out.Operations = unknown("lxd_operations_changed")
		}
		return nil
	})
	if err != nil {
		return unknownAll()
	}
	return out
}

// Compare only identity facts and current lifecycle facts, never arbitrary
// configuration or response bodies. Sorting removes API inventory ordering.
func inventoryIdentity(facts []string) string {
	sort.Strings(facts)
	raw, _ := json.Marshal(facts)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func instanceInventory(c lxd.InstanceServer, r receipt) (result resourceObservation) {
	facts := []string{}
	defer func() { result.identity = inventoryIdentity(facts) }()
	all, err := c.GetInstancesFull(lxd.GetInstancesFullArgs{AllProjects: true})
	if err != nil || all == nil || len(all) > 10000 {
		return unknown("lxd_full_instances_unavailable")
	}
	seen := map[string]bool{}
	uuids := map[string]bool{}
	count := 0
	target := false
	mismatch := false
	networkUnavailable := false
	b := r.Observed.Guard
	for _, vm := range all {
		key := vm.Project + "/" + vm.Name
		id := vm.Config["volatile.uuid"]
		if vm.Project == "" || vm.Name == "" || !uuidPattern.MatchString(id) || seen[key] || uuids[id] || (vm.Type != "virtual-machine" && vm.Type != "container") {
			return unknown("lxd_full_instances_invalid")
		}
		seen[key] = true
		uuids[id] = true
		facts = append(facts, strings.Join([]string{key, id, vm.Type, vm.Location, vm.Config["user.runner-controller-id"], vm.Config["user.runner-pool-id"], vm.Config["volatile.base_image"], strings.Join(vm.Profiles, ","), vm.Status}, "\x00"))
		if vm.Project == "halo-ci" {
			count += 1 + len(vm.Snapshots) + len(vm.Backups)
		}
		if id == b.UUID {
			target = true
			if vm.Project != "halo-ci" || vm.Name != b.Name || vm.Type != "virtual-machine" || vm.Config["user.runner-controller-id"] != b.Controller || vm.Config["user.runner-pool-id"] != b.Pool || vm.Config["volatile.base_image"] != b.Image || len(vm.Profiles) != 1 || vm.Profiles[0] != "halo-ci" || vm.Location != r.Observed.VM.VolumeLocation {
				mismatch = true
			}
			if err := machineAttachments(c.UseProject(vm.Project), vm.Instance); err != nil {
				if errors.Is(err, errNetworkUnavailable) {
					networkUnavailable = true
				} else {
					mismatch = true
				}
			}
		}
		if vm.Project == "halo-ci" && vm.Name == b.Name && id != b.UUID {
			mismatch = true
		}
		snapshots := map[string]bool{}
		for _, snapshot := range vm.Snapshots {
			if snapshot.Name == "" || snapshots[snapshot.Name] || !uuidPattern.MatchString(snapshot.Config["volatile.uuid"]) {
				return unknown("lxd_snapshot_identity_missing")
			}
			snapshots[snapshot.Name] = true
			if snapshot.Config["volatile.uuid"] == b.UUID {
				mismatch = true
			}
			facts = append(facts, key+"\x00snapshot\x00"+snapshot.Name+"\x00"+snapshot.Config["volatile.uuid"])
		}
		backups := map[string]bool{}
		for _, backup := range vm.Backups {
			if backup.Name == "" || backups[backup.Name] {
				return unknown("lxd_backup_identity_missing")
			}
			backups[backup.Name] = true
			facts = append(facts, key+"\x00backup\x00"+backup.Name)
		}
	}
	if mismatch {
		return resource("mismatch", count)
	}
	if networkUnavailable {
		return unknown("lxd_network_unavailable")
	}
	if count == 0 && !target {
		return resource("absent_verified", 0)
	}
	if target && count == 1 {
		return resource("present", 1)
	}
	return resource("residual", count)
}

func volumeInventory(c lxd.InstanceServer, r receipt) (result resourceObservation) {
	facts := []string{}
	defer func() { result.identity = inventoryIdentity(facts) }()
	all, err := c.GetStoragePoolVolumesAllProjects("halo-ci")
	if err != nil || all == nil || len(all) > 10000 {
		return unknown("lxd_all_volumes_unavailable")
	}
	seen := map[string]bool{}
	uuids := map[string]bool{}
	count := 0
	target := false
	mismatch := false
	original := r.Observed.VM
	for _, v := range all {
		key := v.Project + "/" + v.Type + "/" + v.Name + "/" + v.Location
		if v.Project == "" || v.Pool != "halo-ci" || v.Type == "" || v.Name == "" || seen[key] {
			return unknown("lxd_volume_inventory_invalid")
		}
		seen[key] = true
		used := append([]string{}, v.UsedBy...)
		sort.Strings(used)
		facts = append(facts, strings.Join([]string{key, v.ContentType, v.Config["volatile.uuid"], strings.Join(used, "\x00")}, "\x00"))
		if v.Type == "image" {
			continue
		}
		id := v.Config["volatile.uuid"]
		if !uuidPattern.MatchString(id) || uuids[id] {
			return unknown("lxd_volume_identity_missing")
		}
		uuids[id] = true
		// The pool is dedicated; writable leftovers in another project are
		// still capacity and cannot disappear from the absence observation.
		count++
		if id == original.VolumeUUID {
			target = true
			if v.Project != original.VolumeProject || v.Pool != original.VolumePool || v.Name != original.VolumeName || v.Type != original.VolumeType || v.ContentType != original.VolumeContent || v.Location != original.VolumeLocation || len(v.UsedBy) != 1 || v.UsedBy[0] != original.VolumeUsedBy {
				mismatch = true
			}
		}
		if v.Project == "halo-ci" && v.Name == original.VolumeName && v.Type == original.VolumeType && id != original.VolumeUUID {
			mismatch = true
		}
		if strings.Contains(v.Name, "/") && v.Project == "halo-ci" && v.Type != "virtual-machine" && v.Type != "container" && v.Type != "custom" {
			return unknown("lxd_volume_type_invalid")
		}
	}
	if mismatch {
		return resource("mismatch", count)
	}
	if count == 0 && !target {
		return resource("absent_verified", 0)
	}
	if target && count == 1 {
		return resource("present", 1)
	}
	return resource("residual", count)
}

func operationInventory(c lxd.InstanceServer) (result resourceObservation) {
	facts := []string{}
	defer func() { result.identity = inventoryIdentity(facts) }()
	all, err := c.GetOperations()
	if err != nil || all == nil || len(all) > 10000 {
		return unknown("lxd_operations_unavailable")
	}
	seen := map[string]bool{}
	pending := 0
	for _, op := range all {
		if !uuidPattern.MatchString(op.ID) || seen[op.ID] {
			return unknown("lxd_operations_invalid")
		}
		seen[op.ID] = true
		facts = append(facts, fmt.Sprintf("%s/%d/%s", op.ID, op.StatusCode, op.Location))
		switch op.StatusCode {
		case api.Success, api.Failure, api.Cancelled:
		default:
			pending++
		}
	}
	if pending > 0 {
		return resource("pending_or_unknown", pending)
	}
	return resource("terminal_inventory_observed", len(all))
}
