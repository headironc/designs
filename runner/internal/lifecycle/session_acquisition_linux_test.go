package lifecycle

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func newSessionAcquisitionFixture(t *testing.T) (sessionHistory, prepareHistorical) {
	t.Helper()
	f := newPrepareFixture(t)
	report := f.reader.observe(context.Background(), strings.Repeat("9", 64))
	if report.Historical == nil {
		t.Fatal("historical fixture unavailable")
	}
	h := sessionHistory{prepare: f.reader, legacySHA: sessionLegacyOperationSHA}
	source := []byte(fmt.Sprintf(`{"boot_id":%q,"controller_invocation":%q,"service":{"ActiveState":"active","SubState":"running","InvocationID":%q,"MainPID":"120","User":"nucleiron","Group":"nucleiron"},"garm_config_sha256":%q,"policy_sha256":%q,"garm_binary_sha256":%q,"credential_id":1,"credential_name":"nucleiron","endpoint_name":"github","credential_updated_at":"2026-10-27T00:00:00Z","payload_sha256":%q,"source_repository_id":%q,"source_owner":"silence-force"}`+"\n", prepareFixtureBoot, prepareFixtureInvocation, prepareFixtureInvocation, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64), prepareOldRepository))
	token := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{"iat":1793115000,"exp":1793115600,"iss":5469697}`)) + ".Zml4dHVyZQ"
	records := map[string][]byte{
		"acquisition.intent.json":       []byte(fmt.Sprintf(`{"operation":"session-20261027-first-connection","repository_id":1432619931,"full_name":"headironc/nucleus","old_login_status":"unknown","old_operation_sha256":%q,"approval_message":"01a17d25-f7e0-7782-bf50-472c1dce74c0"}`+"\n", sessionLegacyOperationSHA)),
		"acquisition.stopped.json":      []byte(`{"complete":false,"cleanup":{"complete":true,"installation_tokens":{"audit-mint":{"state":"not_attempted","reason":"no_mint_receipt"},"consumer-mint":{"state":"not_attempted","reason":"no_mint_receipt"}},"child_sessions_revoked":false}}` + "\n"),
		"source.json":                   source,
		"app-sign.intent.json":          []byte(fmt.Sprintf(`{"target":"local:Nucleiron","payload_sha256":%q,"started_at":1793115000.125}`+"\n", bytesSHA(source[:len(source)-1]))),
		"app-sign.result.json":          []byte(fmt.Sprintf(`{"phase":"response_received","response":{"app_id":5469697,"installation_id":175207088,"expires_at":1793115600,"token":%q}}`+"\n", token)),
		"app.read.json":                 []byte("{\"status\":200,\"body\":\"fixture GET only\"}\n"),
		"installation-before.read.json": []byte("{\"status\":404,\"body\":\"fixture GET only\"}\n"),
	}
	for name, raw := range records {
		testWrite(t, filepath.Join(h.prepare.parent, name), raw, 0600)
	}
	return h, *report.Historical
}

func TestSessionAcquisitionPreMintHistory(t *testing.T) {
	for _, mode := range []string{"original", "active_app_jwt", "mint_intent", "mint_unknown", "login_intent", "complete", "pending", "unknown_entry", "claimed_cleanup", "revocation_claim", "source_hash", "wrong_boot", "wrong_installation", "unknown_sign", "missing_stopped", "drift"} {
		t.Run(mode, func(t *testing.T) {
			h, historical := newSessionAcquisitionFixture(t)
			now := int64(1793200000)
			replace := func(name, old, next string) {
				t.Helper()
				path := filepath.Join(h.prepare.parent, name)
				raw, err := os.ReadFile(path)
				if err != nil || !bytes.Contains(raw, []byte(old)) {
					t.Fatal("fixture replacement", name, err)
				}
				testWrite(t, path, bytes.ReplaceAll(raw, []byte(old), []byte(next)), 0600)
			}
			switch mode {
			case "active_app_jwt":
				now = 1793115500
			case "mint_intent":
				testWrite(t, filepath.Join(h.prepare.parent, "audit-mint.intent.json"), []byte("{}\n"), 0600)
			case "mint_unknown":
				testWrite(t, filepath.Join(h.prepare.parent, "consumer-mint.result.json"), []byte("{\"phase\":\"unknown\"}\n"), 0600)
			case "login_intent":
				testWrite(t, filepath.Join(h.prepare.parent, "garm-login.intent.json"), []byte("{}\n"), 0600)
			case "complete":
				testWrite(t, filepath.Join(h.prepare.parent, "acquisition.complete.json"), []byte("{}\n"), 0600)
			case "pending":
				testWrite(t, filepath.Join(h.prepare.parent, "app-sign.result.json.pending"), []byte("{}\n"), 0600)
			case "unknown_entry":
				testWrite(t, filepath.Join(h.prepare.parent, "other.json"), []byte("{}\n"), 0600)
			case "claimed_cleanup":
				replace("acquisition.stopped.json", "not_attempted", "settled")
			case "revocation_claim":
				replace("acquisition.stopped.json", `"child_sessions_revoked":false`, `"child_sessions_revoked":true`)
			case "source_hash":
				replace("source.json", "2026-10-27", "2026-10-28")
			case "wrong_boot":
				historical.Boot = "11111111-1111-4111-8111-111111111111"
			case "wrong_installation":
				replace("app-sign.result.json", "175207088", "175824179")
			case "unknown_sign":
				replace("app-sign.result.json", "response_received", "unknown")
			case "missing_stopped":
				if err := os.Remove(filepath.Join(h.prepare.parent, "acquisition.stopped.json")); err != nil {
					t.Fatal(err)
				}
			}
			s := newPrepareFiles(h.prepare.files)
			defer s.close()
			sha, err := h.acquisition(s, historical, now)
			allowed := mode == "original" || mode == "drift"
			if (err == nil) != allowed || allowed && !digestPattern.MatchString(sha) {
				t.Fatal("acquisition admission", sha, err)
			}
			if mode == "drift" {
				testWrite(t, filepath.Join(h.prepare.parent, "audit-mint.intent.json"), []byte("{}\n"), 0600)
				if s.unchanged(context.Background()) {
					t.Fatal("new historical write did not invalidate lease")
				}
			}
		})
	}
}

func TestSessionAppOriginalExpiryProjection(t *testing.T) {
	for _, tc := range []struct {
		claims string
		valid  bool
	}{
		{`{"iat":100,"exp":200,"iss":5469697}`, true},
		{`{"iat":100,"exp":200,"iss":"5469697"}`, true},
		{`{"iat":100,"exp":200,"iss":5469697,"nbf":100}`, true},
		{`{"iat":200,"exp":200,"iss":5469697}`, false},
		{`{"iat":100,"exp":201,"iss":5469697}`, false},
		{`{"iat":100,"exp":200,"iss":5469698}`, false},
		{`{"iat":100,"exp":200,"EXP":200,"iss":5469697}`, false},
		{`{"iat":100,"exp":null,"iss":5469697}`, false},
		{`{"iat":100,"EXP":200,"iss":5469697}`, false},
	} {
		token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(tc.claims)) + ".Zml4dHVyZQ"
		if expiredSessionAppToken(token, 200, 5469697) != tc.valid {
			t.Fatal("original expiry projection", tc.claims)
		}
	}
	if validSessionEpoch("1793200000.000000001", 1793200000) || validSessionEpoch("1e999999999999", 1793200000) || validSessionEpoch("1e-999999999999", 1793200000) {
		t.Fatal("epoch rounding or unbounded exponent accepted")
	}
}

func TestSessionAcquisitionRequiredServiceFields(t *testing.T) {
	h, historical := newSessionAcquisitionFixture(t)
	original, err := os.ReadFile(filepath.Join(h.prepare.parent, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{`"Group":null`, `"Other":"nucleiron"`, `"Group":0`, `"Group":"other"`} {
		raw := bytes.Replace(original, []byte(`"Group":"nucleiron"`), []byte(replacement), 1)
		var source sessionAcquisitionSource
		if strictObject(raw, &source) == nil && validSessionAcquisitionSource(source, historical) {
			t.Fatal("malformed required service field admitted", replacement)
		}
	}
}

func TestSessionAdmissionHistoryComposesOriginalLeases(t *testing.T) {
	for _, mode := range []string{"original", "later_mint", "clock_back", "expired_plan"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := newSessionAcquisitionFixture(t)
			h.legacyDir = filepath.Join(t.TempDir(), "legacy")
			raw := sessionLegacyFixture()
			h.legacySHA = bytesSHA(raw)
			testWrite(t, filepath.Join(h.legacyDir, "operation.json"), raw, 0600)
			testWrite(t, filepath.Join(h.legacyDir, "original-state.json"), []byte("{}\n"), 0600)
			testWrite(t, filepath.Join(h.legacyDir, "owner.lock"), nil, 0600)
			path := filepath.Join(h.prepare.parent, "acquisition.intent.json")
			intent, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			testWrite(t, path, bytes.ReplaceAll(intent, []byte(sessionLegacyOperationSHA), []byte(h.legacySHA)), 0600)
			now := int64(1793200000)
			plan := sessionPlan{Controller: prepareController, Expires: now + 300}
			called := false
			err = h.withAdmissionHistory(context.Background(), strings.Repeat("9", 64), plan, func() time.Time { return time.Unix(now, 0) }, func(sha string, check func() error) error {
				called = true
				if !digestPattern.MatchString(sha) || check() != nil {
					t.Fatal("original aggregate unavailable")
				}
				for _, path := range []string{filepath.Join(h.prepare.parent, "owner.lock"), filepath.Join(h.legacyDir, "owner.lock")} {
					lock, err := os.OpenFile(path, os.O_RDONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer lock.Close()
					if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
						t.Fatal("original lock escaped composed lease", err)
					}
				}
				switch mode {
				case "later_mint":
					testWrite(t, filepath.Join(h.prepare.parent, "audit-mint.intent.json"), []byte("{}\n"), 0600)
				case "clock_back":
					now--
				case "expired_plan":
					now = plan.Expires
				}
				return check()
			})
			if !called || (err == nil) != (mode == "original") {
				t.Fatal("composed historical admission", called, err)
			}
		})
	}
}
