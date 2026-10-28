package lifecycle

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"

	"github.com/headironc/designs/runner/internal/watchdog"
)

type sessionWatchdog struct {
	files          protectedFiles
	parent, policy string
}

// Pause and watchdog execution have different original permanent locks. Keep
// both, in their established order, so a new pause cannot race this window and
// a timer tick cannot log in while its original StopUnit is being observed.
func (w sessionWatchdog) withIdleState(ctx context.Context, controller string, consume func(string, func() error) error) error {
	if consume == nil || !uuidPattern.MatchString(controller) || ctx.Err() != nil {
		return errRefused
	}
	pause, err := openPauseStore(w.files, w.parent, true)
	if err != nil {
		return errRefused
	}
	defer pause.close()
	if exists, err := pause.existing(); err != nil || exists {
		// A separate original pause keeps its execution and recovery owner.
		return errRefused
	}
	names, err := pause.parent.Readdirnames(4097)
	if err != nil && err != io.EOF || len(names) > 4096 {
		return errRefused
	}
	for _, name := range names {
		if name != "state.json" && name != "operation.lock" && name != "pause.lock" {
			// In particular, never create a lock beside an original stop
			// request whose operation.lock has disappeared.
			return errRefused
		}
	}
	// state.json is an occupancy sentinel: its presence forbids creating a
	// replacement operation.lock even if the original lock has disappeared.
	operation, err := openServiceFiles(w.files, w.parent, "state.json", "operation.lock", true)
	if err != nil {
		return errRefused
	}
	defer operation.close()
	s := newPrepareFiles(w.files)
	defer s.close()
	dir, err := s.open(w.parent, true, 0700, false)
	if err != nil || dir == nil {
		return errRefused
	}
	for _, name := range dir.names {
		if name != "state.json" && name != "operation.lock" && name != "pause.lock" {
			return errRefused
		}
	}
	state, err := s.read(filepath.Join(w.parent, "state.json"), 8192)
	defer clear(state)
	if err != nil || state != nil && unambiguous(state) != nil {
		return errRefused
	}
	policy, err := s.readMode(w.policy, 0644, 65536)
	defer clear(policy)
	if err != nil || policy == nil || unambiguous(policy) != nil || watchdog.CheckIdleState(state, policy, controller) != nil {
		return errRefused
	}
	raw, err := json.Marshal(map[string]string{"watchdog_state": bytesSHA(state), "watchdog_policy": bytesSHA(policy)})
	if err != nil {
		return errRefused
	}
	check := func() error {
		if ctx.Err() != nil || !pause.unchanged() || !operation.unchanged() || !s.unchanged(ctx) {
			return errRefused
		}
		return nil
	}
	if check() != nil {
		return errRefused
	}
	return consume(bytesSHA(raw), check)
}
