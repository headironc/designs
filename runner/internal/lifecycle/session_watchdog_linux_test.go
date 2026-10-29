package lifecycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const sessionWatchdogPolicy = `{"controller_id":"7c78121a-33f1-41bd-a3a2-d187cb00b590","repository_id":"6f1b8cfe-ec05-49a6-a4f5-8dcf2866899d","scaleset_name":"nucleiron-halo","provider_owned_field":"retained"}`
const sessionWatchdogState = `{"version":1,"binding":{"controller_id":"7c78121a-33f1-41bd-a3a2-d187cb00b590","repository_id":"6f1b8cfe-ec05-49a6-a4f5-8dcf2866899d","scaleset_name":"nucleiron-halo"},"login_pending":false,"disable_pending":0}`

func TestSessionWatchdogOriginalStateLease(t *testing.T) {
	for _, mode := range []string{"first_use", "new", "idle", "login_pending", "disable_pending", "stop_request", "stop_missing_lock", "old_pause", "pending", "foreign_policy", "missing_lock", "busy_operation", "busy_pause", "changed", "new_stop", "replaced_lock", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			w := sessionWatchdog{files: protectedFiles{uid: uint32(os.Getuid()), boundary: root}, parent: filepath.Join(root, "watchdog"), policy: filepath.Join(root, "policy.json")}
			testWrite(t, w.policy, []byte(sessionWatchdogPolicy), 0644)
			testWrite(t, filepath.Join(w.parent, "operation.lock"), nil, 0600)
			testWrite(t, filepath.Join(w.parent, "pause.lock"), nil, 0600)
			state := []byte(sessionWatchdogState)
			switch mode {
			case "login_pending":
				state = bytes.Replace(state, []byte(`"login_pending":false`), []byte(`"login_pending":true`), 1)
			case "disable_pending":
				state = bytes.Replace(state, []byte(`"disable_pending":0`), []byte(`"disable_pending":81`), 1)
			case "foreign_policy":
				testWrite(t, w.policy, []byte(strings.Replace(sessionWatchdogPolicy, "nucleiron-halo", "other-halo", 1)), 0644)
			}
			if mode != "new" && mode != "first_use" && mode != "stop_missing_lock" {
				testWrite(t, filepath.Join(w.parent, "state.json"), state, 0600)
			}
			switch mode {
			case "stop_request", "stop_missing_lock":
				testWrite(t, filepath.Join(w.parent, "stop-request.json"), []byte(`{"version":1}`), 0600)
				if mode == "stop_missing_lock" {
					if err := os.Remove(filepath.Join(w.parent, "operation.lock")); err != nil {
						t.Fatal(err)
					}
				}
			case "first_use":
				for _, name := range []string{"operation.lock", "pause.lock"} {
					if err := os.Remove(filepath.Join(w.parent, name)); err != nil {
						t.Fatal(err)
					}
				}
			case "pending":
				testWrite(t, filepath.Join(w.parent, ".receipt-interrupted"), state, 0600)
			case "old_pause":
				testWrite(t, filepath.Join(w.parent, "pause", "intent.json"), []byte(`{}`), 0600)
			case "missing_lock":
				if err := os.Remove(filepath.Join(w.parent, "operation.lock")); err != nil {
					t.Fatal(err)
				}
			case "busy_operation", "busy_pause":
				name := "operation.lock"
				if mode == "busy_pause" {
					name = "pause.lock"
				}
				lock, err := os.OpenFile(filepath.Join(w.parent, name), os.O_RDONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := false
			err := w.withIdleState(ctx, prepareController, func(sha string, check func() error) error {
				called = true
				if !digestPattern.MatchString(sha) || check() != nil {
					t.Fatal("original idle state unavailable")
				}
				for _, name := range []string{"pause.lock", "operation.lock"} {
					lock, err := os.OpenFile(filepath.Join(w.parent, name), os.O_RDONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer lock.Close()
					if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
						t.Fatal("original lock escaped callback", name, err)
					}
				}
				switch mode {
				case "changed":
					testWrite(t, filepath.Join(w.parent, "state.json"), []byte(`{}`), 0600)
				case "new_stop":
					testWrite(t, filepath.Join(w.parent, "stop-request.json"), []byte(`{}`), 0600)
				case "replaced_lock":
					path := filepath.Join(w.parent, "operation.lock")
					if err := os.Rename(path, path+".original"); err != nil {
						t.Fatal(err)
					}
					testWrite(t, path, nil, 0600)
				case "cancel":
					cancel()
				}
				return check()
			})
			wantCalled := mode == "first_use" || mode == "new" || mode == "idle" || mode == "changed" || mode == "new_stop" || mode == "replaced_lock" || mode == "cancel"
			if called != wantCalled || (err == nil) != (mode == "first_use" || mode == "new" || mode == "idle") {
				t.Fatal("watchdog window admission", called, err)
			}
			if mode == "missing_lock" || mode == "stop_missing_lock" {
				if _, err := os.Stat(filepath.Join(w.parent, "operation.lock")); !os.IsNotExist(err) {
					t.Fatal("recreated historical operation lock", err)
				}
			}
		})
	}
}
