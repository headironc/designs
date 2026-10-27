package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type guardState struct {
	Version int             `json:"version"`
	Active  json.RawMessage `json:"active"`
	Intent  json.RawMessage `json:"intent"`
	Latch   json.RawMessage `json:"latch"`
}

type activeInstance struct {
	Name    string      `json:"name"`
	UUID    string      `json:"uuid"`
	Ready   *bool       `json:"ready"`
	At      json.Number `json:"at"`
	AgentID *int64      `json:"agent_id,omitempty"`
}

type latch struct {
	Reason           string      `json:"reason"`
	At               json.Number `json:"at"`
	BlocksDelete     *bool       `json:"blocks_delete,omitempty"`
	OperationFailure string      `json:"operation_failure,omitempty"`
}

type health struct {
	capacity func(context.Context) error
	ready    func(context.Context, guardPolicy, activeInstance) (*int64, error)
	now      func() time.Time
}

type fault struct {
	reason       string
	blocksDelete bool
}

func (f fault) Error() string { return f.reason }

// CheckWatchdog observes health and durably records ready/latch transitions.
func CheckWatchdog(ctx context.Context, output io.Writer) int {
	s, err := servicePaths()
	stage := "identity"
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, 85*time.Second)
		defer cancel()
		stage, err = s.watchdog(ctx, health{capacity: hostCapacity, ready: observeReady, now: time.Now})
	}
	if errors.Is(err, errBusy) {
		stage = "busy"
	}
	if err != nil {
		fmt.Fprintf(output, "{\"event\":\"guard_watchdog_refused\",\"stage\":%q}\n", stage)
		if errors.Is(err, errBusy) {
			return 75
		}
		return 1
	}
	return 0
}

func (s startup) watchdog(ctx context.Context, h health) (string, error) {
	return s.withState(func(g *guardFiles) error {
		var p guardPolicy
		if decodeObject(g.policy, &p, true) != nil || !p.valid() {
			return errRefused
		}
		var state guardState
		if decodeObject(g.raw, &state, false) != nil || state.Version != 1 {
			return errRefused
		}
		// Validate active using the same admission invariant as normal startup.
		probe := state
		probe.Intent, probe.Latch = json.RawMessage("null"), json.RawMessage("null")
		raw, _ := json.Marshal(probe)
		if !normalState(raw) {
			return errRefused
		}
		var existing latch
		if !isNull(state.Latch) && (decodeObject(state.Latch, &existing, false) != nil || existing.Reason == "" || !validTimestamp(state.Latch)) {
			return errRefused
		}
		if !isNull(state.Latch) {
			var fields map[string]json.RawMessage
			if json.Unmarshal(state.Latch, &fields) != nil || isNull(fields["blocks_delete"]) {
				return errRefused
			}
		}
		if !isNull(state.Intent) {
			var intent struct {
				Command string      `json:"command"`
				Name    string      `json:"name"`
				At      json.Number `json:"at"`
			}
			if decodeObject(state.Intent, &intent, false) != nil || (intent.Command != "CreateInstance" && intent.Command != "DeleteInstance") || !instanceName.MatchString(intent.Name) || !validTimestamp(state.Intent) {
				return errRefused
			}
			return g.trip(ctx, &state, existing, fault{"Interrupted provider operation requires reconciliation", true}, h.now())
		}
		if !isNull(state.Latch) {
			return errRefused
		}
		if err := h.capacity(ctx); err != nil {
			return g.tripError(ctx, &state, err, h.now())
		}
		if isNull(state.Active) {
			return ctx.Err()
		}
		var active activeInstance
		if decodeObject(state.Active, &active, false) != nil {
			return errRefused
		}
		if *active.Ready {
			return ctx.Err()
		}
		agent, err := h.ready(ctx, p, active)
		if err != nil {
			return g.tripError(ctx, &state, err, h.now())
		}
		if agent == nil {
			at, _ := active.At.Float64()
			if float64(h.now().UnixNano())/1e9-at >= 600 {
				return g.trip(ctx, &state, latch{}, fault{"Bootstrap deadline exceeded", true}, h.now())
			}
			return ctx.Err()
		}
		if *agent <= 0 {
			return g.trip(ctx, &state, latch{}, fault{"Bootstrap completion record invalid", true}, h.now())
		}
		ready := true
		active.Ready, active.AgentID = &ready, agent
		state.Active, _ = json.Marshal(active)
		next, _ := json.Marshal(state)
		return g.persist(ctx, next, g.dir.Sync)
	})
}

func validTimestamp(raw []byte) bool {
	var value struct {
		At float64 `json:"at"`
	}
	return decodeObject(raw, &value, true) == nil && value.At > 0
}

func (g *guardFiles) tripError(ctx context.Context, state *guardState, err error, now time.Time) error {
	f := fault{"Watchdog observation failed", false}
	var observed fault
	if errors.As(err, &observed) {
		f = observed
	}
	return g.trip(ctx, state, latch{}, f, now)
}

func (g *guardFiles) trip(ctx context.Context, state *guardState, old latch, f fault, now time.Time) error {
	if old.Reason == "" {
		old = latch{Reason: f.reason, At: json.Number(fmt.Sprintf("%.9f", float64(now.UnixNano())/1e9)), BlocksDelete: &f.blocksDelete}
	} else if f.blocksDelete && old.BlocksDelete != nil && !*old.BlocksDelete {
		old.BlocksDelete, old.OperationFailure = &f.blocksDelete, f.reason
	}
	state.Latch, _ = json.Marshal(old)
	next, _ := json.Marshal(state)
	if err := g.persist(ctx, next, g.dir.Sync); err != nil {
		return err
	}
	return errRefused
}
