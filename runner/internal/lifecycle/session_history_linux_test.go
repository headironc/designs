package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func sessionLegacyFixture() []byte {
	return []byte(fmt.Sprintf(`{"script_sha256":%q,"helper_sha256":%q,"original_sha256":%q,"phase":"restart-intent","jobs":{"initial-stop":{"method":"StopUnit","mode":"fail","status":"terminal","started_monotonic_us":123,"path":"/org/freedesktop/systemd1/job/81","result":"done"},"test-start":{"method":"StartUnit","mode":"fail","status":"rejected","started_monotonic_us":124,"error_name":"org.freedesktop.systemd1.StartLimitHit"},"restore-start":{"method":"StartUnit","mode":"fail","status":"terminal","started_monotonic_us":125,"path":"/org/freedesktop/systemd1/job/82","result":"done"}},"job_events":{"/org/freedesktop/systemd1/job/81":{"id":81,"unit":"nucleiron.service","result":"done"},"/org/freedesktop/systemd1/job/82":{"id":82,"unit":"nucleiron.service","result":"done"}},"reset_failed":"done","unprojected_original_fact":{"old_login":"unknown"}}`, sessionLegacyScriptSHA, strings.Repeat("2", 64), bytesSHA([]byte("{}\n"))))
}

func TestSessionLegacyOriginalServiceOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, old, value string
		allowed          bool
	}{
		{"original", "", "", true},
		{"restored_is_not_login_proof", `"phase":"restart-intent"`, `"phase":"restored"`, true},
		{"ended_failure", `"result":"done"`, `"result":"failed"`, true},
		{"later_original_event", `"status":"terminal"`, `"status":"terminal","readback":{"active":"inactive"}`, true},
		{"missing_event", `"id":81`, `"id":83`, false},
		{"event_disagrees", `"unit":"nucleiron.service","result":"done"`, `"unit":"nucleiron.service","result":"failed"`, false},
		{"wrong_unit", `nucleiron.service`, `other.service`, false},
		{"unbound_intent", `"status":"terminal"`, `"status":"send-intent"`, false},
		{"lost_reply", `"status":"terminal"`, `"status":"unknown"`, false},
		{"readback_only", `"status":"terminal"`, `"status":"observed-stopped"`, false},
		{"unknown_reset", `"reset_failed":"done"`, `"reset_failed":"send-intent"`, false},
		{"null_reset", `"reset_failed":"done"`, `"reset_failed":null`, false},
		{"unknown_rejection", `org.freedesktop.systemd1.StartLimitHit`, `org.freedesktop.DBus.Error.Timeout`, false},
		{"wrong_method", `"method":"StopUnit"`, `"method":"StartUnit"`, false},
		{"wrong_mode", `"mode":"fail"`, `"mode":"replace"`, false},
		{"unknown_label", `"initial-stop"`, `"extra-stop"`, false},
		{"null_started", `"started_monotonic_us":123`, `"started_monotonic_us":null`, false},
		{"duplicate", `"phase":"restart-intent"`, `"phase":"restart-intent","phase":"restored"`, false},
		{"alias", `"phase":"restart-intent"`, `"phase":"restart-intent","PHASE":"restored"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := sessionLegacyFixture()
			if tc.old != "" {
				raw = bytes.ReplaceAll(raw, []byte(tc.old), []byte(tc.value))
			}
			if err := sessionLegacyJobsSettled(raw); (err == nil) != tc.allowed {
				t.Fatalf("service outcome admitted=%v, want %v", err == nil, tc.allowed)
			}
		})
	}
}

func TestSessionHistoryOriginalLocksAndFrozenBytes(t *testing.T) {
	for _, mode := range []string{"original", "busy", "missing_lock", "wrong_digest", "unknown_job", "wrong_state", "changed_during_lease", "changed_prepare", "wrong_controller"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrepareFixture(t)
			legacy := filepath.Join(t.TempDir(), "legacy")
			raw := sessionLegacyFixture()
			if mode == "unknown_job" {
				raw = bytes.ReplaceAll(raw, []byte(`"status":"terminal"`), []byte(`"status":"observed-running"`))
			}
			testWrite(t, filepath.Join(legacy, "operation.json"), raw, 0600)
			testWrite(t, filepath.Join(legacy, "original-state.json"), []byte("{}\n"), 0600)
			testWrite(t, filepath.Join(legacy, "owner.lock"), nil, 0600)
			h := sessionHistory{prepare: f.reader, legacyDir: legacy, legacySHA: bytesSHA(raw)}
			lock, err := os.OpenFile(filepath.Join(legacy, "owner.lock"), os.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			controller := "7c78121a-33f1-41bd-a3a2-d187cb00b590"
			switch mode {
			case "busy":
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			case "missing_lock":
				if err := os.Remove(filepath.Join(legacy, "owner.lock")); err != nil {
					t.Fatal(err)
				}
			case "wrong_digest":
				h.legacySHA = strings.Repeat("7", 64)
			case "wrong_state":
				testWrite(t, filepath.Join(legacy, "original-state.json"), []byte("[]\n"), 0600)
			case "wrong_controller":
				controller = "11111111-1111-4111-8111-111111111111"
			}
			called := false
			err = h.withServiceHistory(context.Background(), strings.Repeat("9", 64), controller, func(historical prepareHistorical, sha string, check func() error) error {
				called = true
				if historical.Controller != controller || sha != bytesSHA(raw) || check() != nil {
					t.Fatal("original provenance missing")
				}
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
					t.Fatal("legacy owner lock was not retained", err)
				}
				if mode == "changed_during_lease" {
					testWrite(t, filepath.Join(legacy, "operation.json"), raw, 0600)
				}
				if mode == "changed_prepare" {
					testWrite(t, filepath.Join(f.amendment, "authorization.intent.json"), []byte("{}\n"), 0600)
				}
				return check()
			})
			wantCalled := mode == "original" || mode == "changed_during_lease" || mode == "changed_prepare"
			if called != wantCalled || (err == nil) != (mode == "original") {
				t.Fatal("historical admission", called, err)
			}
			if mode != "missing_lock" {
				got, err := os.ReadFile(filepath.Join(legacy, "operation.json"))
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatal("history observer changed original bytes")
				}
			}
		})
	}
}
