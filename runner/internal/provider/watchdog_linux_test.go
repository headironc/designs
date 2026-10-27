package provider

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const controllerID = "11111111-2222-4333-8444-555555555555"
const poolID = "22222222-2222-4333-8444-555555555555"

func watchdogFixture(t *testing.T) (startup, health) {
	t.Helper()
	s := fixture(t)
	p := map[string]string{"provider_sha256": fileSHA(t, s.native), "config_sha256": fileSHA(t, s.config), "controller_id": controllerID, "pool_id": poolID, "image_fingerprint": strings.Repeat("a", 64)}
	raw, _ := json.Marshal(p)
	write(t, s.policy, raw, 0644)
	return s, health{capacity: func(context.Context) error { return nil }, ready: func(context.Context, guardPolicy, activeInstance) (*int64, error) { return nil, nil }, now: func() time.Time { return time.Unix(1793000000, 125000000) }}
}

func loadState(t *testing.T, s startup) guardState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.state, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state guardState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestWatchdogTransitions(t *testing.T) {
	for _, tc := range []struct {
		name, initial string
		age           int
		agent         int64
		failure       error
		blocked       *bool
		success       bool
	}{
		{name: "idle", initial: clearState, success: true},
		{name: "pending 599", initial: strings.Replace(clearState, `"active":null`, `"active":`+activeState, 1), age: 599, success: true},
		{name: "deadline", initial: strings.Replace(clearState, `"active":null`, `"active":`+activeState, 1), age: 600, blocked: boolPtr(true)},
		{name: "ready exact integer", initial: strings.Replace(clearState, `"active":null`, `"active":`+activeState, 1), agent: 9007199254740993, success: true},
		{name: "capacity", initial: clearState, failure: fault{"Storage reserve exhausted", false}, blocked: boolPtr(false)},
		{name: "observation error", initial: clearState, failure: errors.New("secret upstream failure"), blocked: boolPtr(false)},
		{name: "unknown create", initial: strings.Replace(clearState, `"intent":null`, `"intent":{"command":"CreateInstance","name":"runner-one","at":1793000000}`, 1), blocked: boolPtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h := watchdogFixture(t)
			write(t, filepath.Join(s.state, "state.json"), []byte(tc.initial), 0600)
			h.now = func() time.Time { return time.Unix(1793000000+int64(tc.age), 125000000) }
			h.capacity = func(context.Context) error { return tc.failure }
			if tc.agent != 0 {
				h.ready = func(context.Context, guardPolicy, activeInstance) (*int64, error) { return &tc.agent, nil }
			}
			_, err := s.watchdog(context.Background(), h)
			if (err == nil) != tc.success {
				t.Fatalf("unexpected result: %v", err)
			}
			state := loadState(t, s)
			var original guardState
			json.Unmarshal([]byte(tc.initial), &original)
			if string(state.Intent) != string(original.Intent) {
				t.Fatal("intent changed")
			}
			if tc.blocked != nil {
				var l latch
				if json.Unmarshal(state.Latch, &l) != nil || l.BlocksDelete == nil || *l.BlocksDelete != *tc.blocked || strings.Contains(string(state.Latch), "secret") {
					t.Fatalf("wrong latch: %s", state.Latch)
				}
				if string(state.Active) != string(original.Active) {
					t.Fatal("trip changed active")
				}
			} else if tc.agent != 0 {
				var a activeInstance
				json.Unmarshal(state.Active, &a)
				if a.Ready == nil || !*a.Ready || a.AgentID == nil || *a.AgentID != tc.agent || a.UUID != controllerID || a.At != "1793000000.125" {
					t.Fatalf("wrong ready: %s", state.Active)
				}
			} else if fileSHA(t, filepath.Join(s.state, "state.json")) != hashBytes([]byte(tc.initial)) {
				t.Fatal("observation rewrote state")
			}
			if _, err := os.Lstat(filepath.Join(s.state, "state.pending")); !os.IsNotExist(err) {
				t.Fatal("completed transaction retained marker")
			}
			if _, err := s.check(); (err == nil) != (tc.blocked == nil) {
				t.Fatalf("startup regression: %v", err)
			}
		})
	}
}

func boolPtr(b bool) *bool      { return &b }
func hashBytes(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func TestLatchUpgradeRetainsOriginalFacts(t *testing.T) {
	s, h := watchdogFixture(t)
	initial := `{"version":1,"active":null,"intent":{"command":"DeleteInstance","name":"runner-one","at":1793000000},"latch":{"reason":"Storage reserve exhausted","at":1792999999.123456789,"blocks_delete":false}}`
	write(t, filepath.Join(s.state, "state.json"), []byte(initial), 0600)
	h.capacity = func(context.Context) error { t.Fatal("observed with intent"); return nil }
	if _, err := s.watchdog(context.Background(), h); err == nil {
		t.Fatal("accepted intent")
	}
	state := loadState(t, s)
	var l latch
	json.Unmarshal(state.Latch, &l)
	if l.Reason != "Storage reserve exhausted" || l.At != "1792999999.123456789" || l.BlocksDelete == nil || !*l.BlocksDelete || l.OperationFailure != "Interrupted provider operation requires reconciliation" {
		t.Fatalf("lost original: %s", state.Latch)
	}
	before := fileSHA(t, filepath.Join(s.state, "state.json"))
	state.Intent = json.RawMessage("null")
	raw, _ := json.Marshal(state)
	write(t, filepath.Join(s.state, "state.json"), raw, 0600)
	before = fileSHA(t, filepath.Join(s.state, "state.json"))
	if _, err := s.watchdog(context.Background(), h); err == nil {
		t.Fatal("accepted latch")
	}
	if before != fileSHA(t, filepath.Join(s.state, "state.json")) {
		t.Fatal("existing latch rewritten")
	}
}

func TestWatchdogNeverReleasesActiveOrReadsReadyTwice(t *testing.T) {
	s, h := watchdogFixture(t)
	active := strings.Replace(activeState, `"ready":false`, `"ready":true,"agent_id":42`, 1)
	write(t, filepath.Join(s.state, "state.json"), []byte(strings.Replace(clearState, `"active":null`, `"active":`+active, 1)), 0600)
	h.ready = func(context.Context, guardPolicy, activeInstance) (*int64, error) {
		t.Fatal("reobserved ready VM")
		return nil, nil
	}
	before := fileSHA(t, filepath.Join(s.state, "state.json"))
	if _, err := s.watchdog(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if before != fileSHA(t, filepath.Join(s.state, "state.json")) {
		t.Fatal("changed active")
	}
}

func TestStateTransactionFailureAndReentry(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			s, h := watchdogFixture(t)
			next := []byte(strings.Replace(clearState, `"active":null`, `"active":`+activeState, 1))
			calls := 0
			_, err := s.withState(func(g *guardFiles) error {
				return g.persist(context.Background(), next, func() error {
					calls++
					if calls == failAt {
						return errors.New("fsync failed")
					}
					return g.dir.Sync()
				})
			})
			if err == nil {
				t.Fatal("save failure became success")
			}
			raw, _ := os.ReadFile(filepath.Join(s.state, "state.json"))
			if failAt == 1 && string(raw) != clearState {
				t.Fatal("changed before commit")
			}
			if failAt >= 2 && string(raw) != string(next) {
				t.Fatal("replacement missing")
			}
			_, err = s.watchdog(context.Background(), h)
			if failAt < 3 && err == nil {
				t.Fatal("reentered interrupted transaction")
			}
			// The last failure follows durable replacement and marker removal.
			if failAt == 3 && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWatchdogRefusesMaintenanceAndInterruptedArtifacts(t *testing.T) {
	for _, name := range []string{"maintenance", "state.pending", "state.new"} {
		t.Run(name, func(t *testing.T) {
			s, h := watchdogFixture(t)
			path := filepath.Join(s.state, name)
			if name == "maintenance" {
				path = s.maintenance
			}
			write(t, path, []byte("retain original"), 0600)
			h.capacity = func(context.Context) error { t.Fatal("observed blocked state"); return nil }
			if _, err := s.watchdog(context.Background(), h); err == nil {
				t.Fatal("accepted")
			}
			if _, err := s.check(); err == nil {
				t.Fatal("startup accepted")
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != "retain original" {
				t.Fatal("original overwritten")
			}
		})
	}
}

func TestInterruptedStateWriterAcrossProcesses(t *testing.T) {
	for _, phase := range []string{"prepared", "renamed"} {
		t.Run(phase, func(t *testing.T) {
			s, h := watchdogFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStateWriterProcess$")
			cmd.Env = append(os.Environ(), "GUARD_TX_ROOT="+filepath.Dir(s.state), "GUARD_TX_PHASE="+phase)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
			line, err := bufio.NewReader(out).ReadString('\n')
			if err != nil || line != "paused\n" {
				t.Fatalf("writer: %q %v", line, err)
			}
			if _, err := s.watchdog(ctx, h); !errors.Is(err, errBusy) {
				t.Fatalf("lost lock: %v", err)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("writer unexpectedly completed")
			}
			if stage, err := s.watchdog(ctx, h); err == nil || stage != "state_transaction" {
				t.Fatalf("unsafe reentry: %s %v", stage, err)
			}
			if stage, err := s.check(); err == nil || stage != "state_transaction" {
				t.Fatalf("startup: %s %v", stage, err)
			}
			state := loadState(t, s)
			if phase == "prepared" && !isNull(state.Active) {
				t.Fatal("wrote before preparation")
			}
			if phase == "renamed" && isNull(state.Active) {
				t.Fatal("lost replacement")
			}
		})
	}
}

func TestStateWriterProcess(t *testing.T) {
	root := os.Getenv("GUARD_TX_ROOT")
	if root == "" {
		return
	}
	s := startup{policy: filepath.Join(root, "policy.json"), native: filepath.Join(root, "native"), config: filepath.Join(root, "config.toml"), state: filepath.Join(root, "state"), maintenance: filepath.Join(root, "maintenance"), rootUID: uint32(os.Geteuid()), serviceUID: uint32(os.Geteuid())}
	count := 0
	_, err := s.withState(func(g *guardFiles) error {
		next := []byte(strings.Replace(clearState, `"active":null`, `"active":`+activeState, 1))
		return g.persist(context.Background(), next, func() error {
			count++
			if err := g.dir.Sync(); err != nil {
				return err
			}
			if count == 1 && os.Getenv("GUARD_TX_PHASE") == "prepared" || count == 2 && os.Getenv("GUARD_TX_PHASE") == "renamed" {
				fmt.Println("paused")
				io.Copy(io.Discard, os.Stdin)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStateWriteRefusesReplacedInput(t *testing.T) {
	s, _ := watchdogFixture(t)
	_, err := s.withState(func(g *guardFiles) error {
		if err := os.Rename(filepath.Join(s.state, "state.json"), filepath.Join(s.state, "original.json")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(s.state, "state.json"), []byte(clearState), 0600)
		return g.persist(context.Background(), []byte(clearState), g.dir.Sync)
	})
	if err == nil {
		t.Fatal("accepted replaced state")
	}
	if _, err := os.Lstat(filepath.Join(s.state, "state.pending")); !os.IsNotExist(err) {
		t.Fatal("wrote on replaced input")
	}
	// The unchanged permanent lock is released on refusal.
	f, err := os.Open(filepath.Join(s.state, "operation.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

func TestWatchdogRejectsMalformedStateWithoutMutation(t *testing.T) {
	for _, fragment := range []string{
		`"intent":null`,
		`"intent":{"command":"CreateInstance","name":"runner-one","at":"1793000000"}`,
		`"intent":{"command":"DeleteInstance","name":"runner-one","at":null}`,
		`"intent":{"command":"Other","name":"runner-one","at":1793000000}`,
		`"latch":{"reason":"original","at":1793000000,"blocks_delete":null}`,
		`"latch":{"reason":"original","at":"1793000000","blocks_delete":false}`,
		`"latch":{"reason":"original","at":1793000000,"blocks_delete":false,"blocks_delete":true}`,
	} {
		t.Run(fragment, func(t *testing.T) {
			s, h := watchdogFixture(t)
			raw := strings.Replace(clearState, `"intent":null`, fragment, 1)
			if fragment == `"intent":null` {
				raw = strings.Replace(clearState, `"latch":null`, `"latch":null,"Latch":null`, 1)
			}
			if strings.HasPrefix(fragment, `"latch"`) {
				raw = strings.Replace(clearState, `"latch":null`, fragment, 1)
			}
			write(t, filepath.Join(s.state, "state.json"), []byte(raw), 0600)
			h.capacity = func(context.Context) error { t.Fatal("observed malformed state"); return nil }
			if _, err := s.watchdog(context.Background(), h); err == nil {
				t.Fatal("accepted malformed state")
			}
			if fileSHA(t, filepath.Join(s.state, "state.json")) != hashBytes([]byte(raw)) {
				t.Fatal("rewrote malformed state")
			}
		})
	}
}
