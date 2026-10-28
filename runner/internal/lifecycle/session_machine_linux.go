package lifecycle

import (
	"context"
	"fmt"
	"strings"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
)

func sessionMachineSnapshot(c lxd.InstanceServer, controller string) (string, error) {
	if !uuidPattern.MatchString(controller) {
		return "", errRefused
	}
	facts := []string{}
	instances, err := c.GetInstancesFull(lxd.GetInstancesFullArgs{AllProjects: true})
	if err != nil || instances == nil || len(instances) > 10000 {
		return "", errRefused
	}
	seen, uuids := map[string]bool{}, map[string]bool{}
	for _, vm := range instances {
		key := vm.Project + "/" + vm.Name
		id := vm.Config["volatile.uuid"]
		if vm.Project == "" || vm.Name == "" || !uuidPattern.MatchString(id) || seen[key] || uuids[id] || vm.Type != "virtual-machine" && vm.Type != "container" || vm.Project == "halo-ci" || vm.Config["user.runner-controller-id"] == controller {
			return "", errRefused
		}
		seen[key], uuids[id] = true, true
		facts = append(facts, strings.Join([]string{"instance", key, id, vm.Type, vm.Location, vm.Status}, "/"))
	}
	volumes, err := c.GetStoragePoolVolumesAllProjects("halo-ci")
	if err != nil || volumes == nil || len(volumes) > 10000 {
		return "", errRefused
	}
	seen = map[string]bool{}
	for _, v := range volumes {
		key := v.Project + "/" + v.Type + "/" + v.Name + "/" + v.Location
		// Every writable volume, snapshot or backup in this dedicated pool
		// blocks the window, including a volume owned by another project.
		if v.Project == "" || v.Pool != "halo-ci" || v.Type != "image" || !digestPattern.MatchString(v.Name) || seen[key] {
			return "", errRefused
		}
		seen[key] = true
		facts = append(facts, "image/"+key)
	}
	operations, err := c.GetOperationsAllProjects()
	if err != nil || operations == nil || len(operations) > 10000 {
		return "", errRefused
	}
	seen = map[string]bool{}
	for _, op := range operations {
		if !uuidPattern.MatchString(op.ID) || seen[op.ID] {
			return "", errRefused
		}
		seen[op.ID] = true
		switch op.StatusCode {
		case api.Success, api.Failure, api.Cancelled:
		default:
			return "", errRefused
		}
		facts = append(facts, fmt.Sprintf("operation/%s/%d/%s", op.ID, op.StatusCode, op.Location))
	}
	return inventoryIdentity(facts), nil
}

// This observation is only one input to credential admission. Empty LXD
// inventories do not close original QEMU, cgroup or unknown creation requests.
func observeSessionMachine(ctx context.Context, socket, controller string) (string, error) {
	identity := ""
	err := withMachineClient(ctx, socket, func(c lxd.InstanceServer) error {
		first, err := sessionMachineSnapshot(c, controller)
		if err != nil {
			return errRefused
		}
		second, err := sessionMachineSnapshot(c, controller)
		if err != nil || first != second {
			return errRefused
		}
		identity = first
		return nil
	})
	if err != nil {
		return "", errRefused
	}
	return identity, nil
}
