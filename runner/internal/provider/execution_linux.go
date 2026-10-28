package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/user"
	"strconv"

	"golang.org/x/sys/unix"
)

// ExecutionBinding is a snapshot of the guard's existing ready allocation.
// It conveys identity only, never permission to mutate or release the allocation.
type ExecutionBinding struct {
	Controller  string `json:"controller_id"`
	Repository  string `json:"repository_id"`
	ScaleSet    string `json:"scaleset_name"`
	Pool        string `json:"pool_id"`
	Image       string `json:"image_fingerprint"`
	Name        string `json:"instance_name"`
	UUID        string `json:"vm_uuid"`
	AgentID     int64  `json:"agent_id"`
	StateSHA    string `json:"state_sha256"`
	PolicySHA   string `json:"policy_sha256"`
	ProviderSHA string `json:"provider_sha256"`
	ConfigSHA   string `json:"config_sha256"`
}

// Valid checks identity reconstruction, without granting admission or mutation.
func (b ExecutionBinding) Valid() bool {
	p := guardPolicy{b.Controller, b.Repository, b.ScaleSet, b.Pool, b.Image}
	return p.valid() && instanceName.MatchString(b.Name) && uuid.MatchString(b.UUID) && b.AgentID > 0 && digest.MatchString(b.StateSHA) && digest.MatchString(b.PolicySHA) && digest.MatchString(b.ProviderSHA) && digest.MatchString(b.ConfigSHA)
}

// InspectExecution holds the permanent provider lock for a root administrative
// observer. The observer prepares a local publication callback; the guard runs
// its final checks before invoking that callback under the same permanent lock.
// The callback must call recheck before publishing its completion seal.
// Normal provider entrypoints retain their separate service-identity requirement.
func InspectExecution(ctx context.Context, observe func(ExecutionBinding, func() error) (func() error, error)) error {
	s, err := administrativePaths()
	if err != nil {
		return err
	}
	return s.inspectExecution(ctx, observe)
}

func administrativePaths() (startup, error) {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return startup{}, errRefused
	}
	identity, err := user.Lookup("nucleiron")
	if err != nil {
		return startup{}, errRefused
	}
	uid, err := strconv.ParseUint(identity.Uid, 10, 32)
	if err != nil || uid == 0 {
		return startup{}, errRefused
	}
	s := startup{policy: "/etc/nucleus-halo/provider-policy.json", native: "/opt/nucleus-halo/bin/garm-provider-lxd", config: "/etc/nucleus-halo/garm-provider-lxd.toml", state: "/var/lib/nucleiron/provider-guard", maintenance: "/run/nucleiron-records-only", serviceUID: uint32(uid)}
	return s, nil
}

func (s startup) inspectExecution(ctx context.Context, observe func(ExecutionBinding, func() error) (func() error, error)) error {
	var publish func() error
	_, err := s.withStatePublication(func(g *guardFiles) error {
		var p readPolicy
		var state guardState
		var active activeInstance
		if decodeObject(g.policy, &p, true) != nil || !p.valid() || !normalState(g.raw) || decodeObject(g.raw, &state, false) != nil || isNull(state.Active) || decodeObject(state.Active, &active, false) != nil || active.Ready == nil || !*active.Ready || active.AgentID == nil || *active.AgentID <= 0 {
			return errRefused
		}
		binding := ExecutionBinding{p.Controller, p.Repository, p.ScaleSet, p.Pool, p.Image, active.Name, active.UUID, *active.AgentID, fmt.Sprintf("%x", sha256.Sum256(g.raw)), fmt.Sprintf("%x", sha256.Sum256(g.policy)), p.ProviderSHA, p.ConfigSHA}
		recheck := func() error {
			if ctx.Err() != nil || !g.unchanged() || absent(s.maintenance) != nil {
				return errRefused
			}
			for _, name := range []string{"state.pending", "state.new"} {
				if absentAt(g.dir, name) != nil {
					return errRefused
				}
			}
			for _, file := range []struct {
				path      string
				uid, mode uint32
				raw       []byte
			}{{s.policy, s.rootUID, 0644, g.policy}, {s.state + "/state.json", s.serviceUID, 0600, g.raw}} {
				raw, err := readPath(file.path, file.uid, file.mode, 64*1024)
				if err != nil || !bytes.Equal(raw, file.raw) {
					return errRefused
				}
			}
			if metadata(g.dir, s.serviceUID, unix.S_IFDIR|0700) != nil || metadata(g.lock, s.serviceUID, unix.S_IFREG|0600) != nil || checkDigest(s.native, s.rootUID, 0755, p.ProviderSHA) != nil || checkDigest(s.config, s.rootUID, 0644, p.ConfigSHA) != nil {
				return errRefused
			}
			return nil
		}
		if recheck() != nil {
			return errRefused
		}
		var err error
		publish, err = observe(binding, recheck)
		if err != nil || publish == nil {
			if err == nil {
				return errRefused
			}
			return err
		}
		return recheck()
	}, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return publish()
	})
	return err
}
