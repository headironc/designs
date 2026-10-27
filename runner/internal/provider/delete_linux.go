package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	lxd "github.com/canonical/lxd/client"
)

type deletePolicy struct {
	guardPolicy
	ProviderSHA string `json:"provider_sha256"`
	ConfigSHA   string `json:"config_sha256"`
}

type deleter struct {
	before func(context.Context, guardPolicy, activeInstance) (*int64, error)
	after  func(context.Context) error
	invoke func(context.Context, *guardFiles, deletePolicy, string) ([]byte, error)
	now    func() time.Time
}

// Delete releases capacity only after one known successful original deletion.
func Delete(ctx context.Context, environment map[string]string, diagnostic io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 330*time.Second)
	defer cancel()
	s, err := servicePaths()
	stage := "identity"
	if err == nil {
		stage, err = s.delete(ctx, environment, deleter{before: beforeDelete, after: afterDelete, invoke: invokeDelete, now: time.Now})
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, errBusy) {
		stage = "busy"
	}
	fmt.Fprintf(diagnostic, "{\"event\":\"guard_delete_refused\",\"stage\":%q}\n", stage)
	if errors.Is(err, errBusy) {
		return 75
	}
	return 1
}

func (s startup) delete(ctx context.Context, env map[string]string, d deleter) (string, error) {
	phase := "admission"
	stage, err := s.withState(func(g *guardFiles) error {
		var p deletePolicy
		if decodeObject(g.policy, &p, true) != nil || !p.valid() {
			return errRefused
		}
		// GARM v0.1.0 deletion supplies no pool or stdin. A supplied pool must
		// still match; target ownership is established from policy and LXD.
		if env["GARM_COMMAND"] != "DeleteInstance" || env["GARM_CONTROLLER_ID"] != p.Controller || env["GARM_PROVIDER_CONFIG_FILE"] != s.config || env["GARM_INTERFACE_VERSION"] != "v0.1.0" || (env["GARM_POOL_ID"] != "" && env["GARM_POOL_ID"] != p.Pool) {
			return errRefused
		}
		var state guardState
		if decodeObject(g.raw, &state, false) != nil || !isNull(state.Intent) {
			return errRefused
		}
		// Capacity/observation latches may permit cleanup, but deletion never
		// clears them or authorizes a subsequent create.
		if !isNull(state.Latch) {
			var block latch
			if decodeObject(state.Latch, &block, false) != nil || block.Reason == "" || !validTimestamp(state.Latch) || block.BlocksDelete == nil || *block.BlocksDelete {
				return errRefused
			}
		}
		probe := state
		probe.Latch = json.RawMessage("null")
		raw, _ := json.Marshal(probe)
		if !normalState(raw) || isNull(state.Active) {
			return errRefused
		}
		var active activeInstance
		if decodeObject(state.Active, &active, false) != nil || env["GARM_INSTANCE_ID"] != active.Name {
			return errRefused
		}
		phase = "instance"
		agent, err := d.before(ctx, p.guardPolicy, active)
		if err != nil || agent == nil || *agent <= 0 || (*active.Ready && *active.AgentID != *agent) {
			return errRefused
		}
		ready := true
		active.Ready, active.AgentID = &ready, agent
		state.Active, _ = json.Marshal(active)
		phase = "intent"
		state.Intent, _ = json.Marshal(struct {
			Command string `json:"command"`
			Name    string `json:"name"`
			At      int64  `json:"at"`
		}{"DeleteInstance", active.Name, d.now().Unix()})
		next, _ := json.Marshal(state)
		if err := g.persist(ctx, next, g.dir.Sync); err != nil {
			return err
		}
		phase = "native"
		result, err := d.invoke(ctx, g, p, active.Name)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(result)) != 0 {
			return errRefused
		}
		phase = "vacancy"
		if err := d.after(ctx); err != nil {
			return err
		}
		phase = "release"
		state.Active, state.Intent = json.RawMessage("null"), json.RawMessage("null")
		next, _ = json.Marshal(state)
		return g.persist(ctx, next, g.dir.Sync)
	})
	if err != nil && stage == "state" {
		stage = phase
	}
	return stage, err
}

func invokeDelete(ctx context.Context, g *guardFiles, p deletePolicy, name string) ([]byte, error) {
	return invokeNative(ctx, g, p.ProviderSHA, p.ConfigSHA, []string{"GARM_COMMAND=DeleteInstance", "GARM_CONTROLLER_ID=" + p.Controller, "GARM_INSTANCE_ID=" + name}, nil)
}

func beforeDelete(ctx context.Context, p guardPolicy, active activeInstance) (*int64, error) {
	var agent *int64
	err := withLXD(ctx, lxdSocket, func(client lxd.InstanceServer) error {
		var err error
		agent, err = deletionTarget(client, p, active)
		return err
	})
	return agent, err
}

func deletionTarget(client lxd.InstanceServer, p guardPolicy, active activeInstance) (*int64, error) {
	id, err := createdInstance(client, p, active.Name)
	if err != nil || id != active.UUID {
		return nil, errRefused
	}
	if *active.Ready {
		return active.AgentID, nil
	}
	return readReady(client, p, active)
}

func afterDelete(ctx context.Context) error {
	return withLXD(ctx, lxdSocket, vacantProject)
}
