package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// GuardObservation exposes current occupancy and refusal flags, never raw state
// or permission to reconcile it. Pending transactions remain read failures.
type GuardObservation struct {
	Stage      string `json:"stage"`
	StateSHA   string `json:"state_sha256,omitempty"`
	PolicySHA  string `json:"policy_sha256,omitempty"`
	Controller string `json:"controller_id,omitempty"`
	Repository string `json:"repository_id,omitempty"`
	ScaleSet   string `json:"scaleset_name,omitempty"`
	Pool       string `json:"pool_id,omitempty"`
	Image      string `json:"image_fingerprint,omitempty"`
	Name       string `json:"active_name,omitempty"`
	UUID       string `json:"active_uuid,omitempty"`
	AgentID    int64  `json:"agent_id,omitempty"`
	Intent     bool   `json:"intent"`
	Latch      bool   `json:"latch"`
}

func ObserveGuard(ctx context.Context) (GuardObservation, error) {
	s, err := administrativePaths()
	if err != nil {
		return GuardObservation{Stage: "identity"}, err
	}
	return s.observeGuard(ctx)
}

func (s startup) observeGuard(ctx context.Context) (GuardObservation, error) {
	var out GuardObservation
	stage, err := s.withState(func(g *guardFiles) error {
		var p guardPolicy
		var state guardState
		if ctx.Err() != nil || decodeObject(g.policy, &p, true) != nil || !p.valid() || decodeObject(g.raw, &state, false) != nil || state.Version != 1 || len(state.Intent) == 0 || len(state.Latch) == 0 {
			return errRefused
		}
		probe := state
		probe.Intent = json.RawMessage("null")
		probe.Latch = json.RawMessage("null")
		raw, _ := json.Marshal(probe)
		if !normalState(raw) {
			return errRefused
		}
		out = GuardObservation{Stage: "observed", StateSHA: fmt.Sprintf("%x", sha256.Sum256(g.raw)), PolicySHA: fmt.Sprintf("%x", sha256.Sum256(g.policy)), Controller: p.Controller, Repository: p.Repository, ScaleSet: p.ScaleSet, Pool: p.Pool, Image: p.Image, Intent: !isNull(state.Intent), Latch: !isNull(state.Latch)}
		if !isNull(state.Active) {
			var a activeInstance
			if decodeObject(state.Active, &a, false) != nil {
				return errRefused
			}
			out.Name, out.UUID = a.Name, a.UUID
			if a.AgentID != nil {
				out.AgentID = *a.AgentID
			}
		}
		current, err := readPath(s.state+"/state.json", s.serviceUID, 0600, 64*1024)
		if err != nil || string(current) != string(g.raw) || ctx.Err() != nil {
			return errRefused
		}
		return nil
	})
	if err != nil {
		return GuardObservation{Stage: stage}, err
	}
	return out, nil
}
