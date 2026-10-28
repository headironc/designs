package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These inputs are synthetic original-format bytes, independent of the reader's
// types and path tables. No archived Python producer is imported or executed.
const prepareFixtureFact = `{"gid":0,"mode":420,"sha256":"1111111111111111111111111111111111111111111111111111111111111111","size":7,"uid":0,"xattrs_sha256":"2222222222222222222222222222222222222222222222222222222222222222"}`
const prepareFixtureInvocation = "63717a25032d4e048d801cbf935a18dc"
const prepareFixtureBoot = "321c9b6c-9304-4c0d-8d2e-9c49b28cc5fb"

func syntheticPrepareFacts() string {
	paths := []string{
		"/etc/nucleiron-egress/dns.yaml", "/etc/nucleiron-egress/squid.conf", "/etc/nucleiron-gateway/gateway.conf", "/etc/nucleiron-gateway/tls/ca.crt", "/etc/nucleiron-gateway/tls/server.crt", "/etc/nucleiron-gateway/tls/server.key", "/etc/nucleus-halo/garm-provider-lxd.toml", "/etc/nucleus-halo/garm.toml", "/etc/nucleus-halo/provider-policy.json", "/etc/rsyslog.d/nucleiron/server.conf",
		"/etc/systemd/system/nucleiron-dns.service", "/etc/systemd/system/nucleiron-egress.service", "/etc/systemd/system/nucleiron-firewall.service", "/etc/systemd/system/nucleiron-gateway.service", "/etc/systemd/system/nucleiron-logs.service", "/etc/systemd/system/nucleiron-provider-watchdog.service", "/etc/systemd/system/nucleiron-provider-watchdog.timer", "/etc/systemd/system/nucleiron.service", "/etc/systemd/system/nucleiron.service.d/30-provider-guard.conf", "/home/headiron/runner-deployment/halo-20261026/nucleiron-ca.crt", "/opt/nucleus-halo/bin/garm", "/opt/nucleus-halo/bin/garm-provider-lxd", "/opt/nucleus-halo/bin/guarded-lxd-provider", "/opt/nucleus-halo/bin/network-firewall.py", "/usr/sbin/nginx",
	}
	rows := []string{}
	for _, path := range paths {
		rows = append(rows, fmt.Sprintf("%q:%s", path, prepareFixtureFact))
	}
	return "{" + strings.Join(rows, ",") + "}"
}
func syntheticPrepareServices() string {
	names := []string{"nucleiron.service", "nucleiron-gateway.service", "nucleiron-provider-watchdog.service", "nucleiron-provider-watchdog.timer", "nucleiron-logs.service", "nucleiron-firewall.service", "nucleiron-dns.service", "nucleiron-egress.service"}
	rows := []string{}
	for _, name := range names {
		view := `{"active":"active","sub":"running","job_id":0,"jobs":[],"invocation":"63717a25032d4e048d801cbf935a18dc"`
		if !strings.HasSuffix(name, ".timer") {
			view += `,"main_pid":120,"control_pid":0,"pids":[120],"result":"success","restarts":0,"start_us":12345678901234567,"exit_us":0,"exit_status":0,"exit_code":0`
		}
		rows = append(rows, fmt.Sprintf("%q:%s}", name, view))
	}
	return "{" + strings.Join(rows, ",") + "}"
}
func prepareTestSHA(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

type prepareTestFixture struct {
	reader    prepareReader
	amendment string
	originals map[string][]byte
}

func newPrepareFixture(t *testing.T) prepareTestFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := prepareReader{files: protectedFiles{uid: uint32(os.Getuid()), boundary: root}, parent: filepath.Join(root, "operation"), release: filepath.Join(root, "release")}
	facts := syntheticPrepareFacts()
	plan := []byte(`{"operation":"session-20261027-first-connection","amendment":"credential-rebind-175824179","approval_message":"01a17dad-af6b-7b41-a2a4-df0a16a24aa1","boot_id":"321c9b6c-9304-4c0d-8d2e-9c49b28cc5fb","controller_id":"7c78121a-33f1-41bd-a3a2-d187cb00b590","old_repository_id":"6f1b8cfe-ec05-49a6-a4f5-8dcf2866899d","repository_id":1432619931,"full_name":"headironc/nucleus","installation_id":175824179,"expires_at":1793118600,"fixed_files":` + facts + "}\n")
	members := []string{"entry.py", "rebind.py", "host.py", "credential", "acquire-session.py", "lifecycle.py", "records_only.py", "first-connection.py", "failure-evidence.py", "request.json", "install-services.py", "install-candidate.py", "adapters/recover-controller-dependencies.py", "adapters/verify-controller-start.py"}
	rows := []string{}
	for _, name := range members {
		mode := os.FileMode(0600)
		if name == "credential" {
			mode = 0500
		}
		testWrite(t, filepath.Join(p.release, name), []byte("old"), mode)
		rows = append(rows, fmt.Sprintf(`{"path":%q,"sha256":"3333333333333333333333333333333333333333333333333333333333333333","size":3}`, name))
	}
	manifest := []byte(`{"approval_message":"01a17dad-af6b-7b41-a2a4-df0a16a24aa1","files":[` + strings.Join(rows, ",") + "]}\n")
	p.releaseSHA, p.planSHA = prepareTestSHA(manifest), prepareTestSHA(plan)
	testWrite(t, filepath.Join(p.release, "runtime-manifest.json"), manifest, 0600)
	testWrite(t, filepath.Join(p.release, "host-plan.json"), plan, 0600)
	a := filepath.Join(p.parent, "credential-rebind-175824179")
	before := []byte(`{"payload":"\u2603fixture-cipher-sensitive","app_id":5469697,"installation_id":175207088,"public_key_sha256":"6a33147cba31aad74310aa9329a89fa0331c1c2def87dabc95593bf0f26b900b","garm_ttl_seconds":86400.5,"repository":{"id":"6f1b8cfe-ec05-49a6-a4f5-8dcf2866899d","owner":"silence-force","name":"nucleus"},"repository_row":["6f1b8cfe-ec05-49a6-a4f5-8dcf2866899d","silence-force","nucleus",1,"github",0,"fixture-webhook-row-sensitive"],"credential_row":[1,"nucleiron","app","fixture-payload-row-sensitive","2026-10-27T00:00:00Z","github","https://api.github.com","https://github.com","github",null,0,"https://uploads.github.com",""],"credential_name":"nucleiron","webhook":"fixture-webhook-sensitive","files":` + facts + `,"controller_invocation":"63717a25032d4e048d801cbf935a18dc"}` + "\n")
	originals := map[string][]byte{
		"repair.intent.json":            []byte("{\"operation\":\"session-20261027-first-connection\",\"amendment\":\"credential-rebind-175824179\",\"approval_message\":\"01a17dad-af6b-7b41-a2a4-df0a16a24aa1\"}\n"),
		"before.json":                   before,
		"services/service-binding.json": []byte(`{"boot_id":"321c9b6c-9304-4c0d-8d2e-9c49b28cc5fb","before":` + facts + `,"after":` + facts + "}\n"),
		"services/services-before.json": []byte(`{"services":` + syntheticPrepareServices() + `,"host":{"files":` + facts + "}}\n"),
		"prepare.complete.json":         []byte(fmt.Sprintf("{\"before_sha256\":%q}\n", prepareTestSHA(before[:len(before)-1]))),
	}
	for name, raw := range originals {
		testWrite(t, filepath.Join(a, name), raw, 0600)
	}
	testWrite(t, filepath.Join(p.parent, "owner.lock"), nil, 0600)
	return prepareTestFixture{p, a, originals}
}
func assertPrepareRestrictions(t *testing.T, r prepareReport) {
	t.Helper()
	if r.OperationComplete || r.Recovered || r.Retry || r.Enable || r.ServiceWrite || r.Cleanup || r.Migration || r.VMAcceptance || r.Current != "not_observed_this_slice" || !reflect.DeepEqual(r.Gaps, []string{"historical_producer_execution_unproven", "service_prestate_original_digest_missing", "external_preflight_not_reperformed", "credentials_not_decrypted_or_used", "current_installed_prestate_not_observed"}) {
		t.Fatalf("observation gained authority: %+v", r)
	}
	raw, e := json.Marshal(r)
	if e != nil || bytes.Contains(raw, []byte("fixture-")) || bytes.Contains(raw, []byte(`\u2603`)) {
		t.Fatal("sensitive projection escaped")
	}
}
func TestPrepareOriginalEvidenceAndReadOnlyReentry(t *testing.T) {
	f := newPrepareFixture(t)
	for i := 0; i < 2; i++ {
		r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
		assertPrepareRestrictions(t, r)
		if r.State != "prepare_records_consistent" || r.Lock.State != "observed" || r.BeforeHash.State != "observed" || r.CrossBinding.State != "observed" || r.Release.State != "metadata_observed" || len(r.Release.Members) != 14 || r.Historical == nil {
			t.Fatalf("original evidence unavailable: %+v", r)
		}
		if r.Historical.Repository != "silence-force/nucleus" || r.Historical.ControllerInvocation != "63717a25032d4e048d801cbf935a18dc" || r.Historical.AppID != 5469697 || r.Historical.Installation != 175207088 || r.Historical.TargetInstallation != 175824179 || r.Historical.Services["nucleiron-provider-watchdog.service"].Main != 120 {
			t.Fatal("independent identity expectation", r.Historical)
		}
		if r.Release.PlanExpires == nil || *r.Release.PlanExpires != 1793118600 {
			t.Fatal("historical expiry was omitted or renewed", r.Release.PlanExpires)
		}
	}
	for name, want := range f.originals {
		got, e := os.ReadFile(filepath.Join(f.amendment, name))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatal("rewrote original", name)
		}
	}
}
func TestPreparePublicationPrefixesNeverPermitRetry(t *testing.T) {
	order := []string{"repair.intent.json", "before.json", "services/service-binding.json", "services/services-before.json", "prepare.complete.json"}
	for n := 0; n < 5; n++ {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := newPrepareFixture(t)
			for _, name := range order[n:] {
				if e := os.Remove(filepath.Join(f.amendment, name)); e != nil {
					t.Fatal(e)
				}
			}
			r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
			assertPrepareRestrictions(t, r)
			if r.State == "prepare_records_consistent" || r.AmendmentDirectory.State != "observed" || r.ServicesDirectory.State != "observed" || r.Release.State != "metadata_observed" {
				t.Fatalf("partial staging erased: %+v", r)
			}
		})
	}
	t.Run("out_of_order", func(t *testing.T) {
		f := newPrepareFixture(t)
		if e := os.Remove(filepath.Join(f.amendment, "before.json")); e != nil {
			t.Fatal(e)
		}
		r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
		if r.Publication.State != "inconsistent" {
			t.Fatal(r.Publication)
		}
		assertPrepareRestrictions(t, r)
	})
}
func TestPrepareSchemaBindingsAndExactBytes(t *testing.T) {
	for _, mode := range []string{"intent_operation", "intent_null", "intent_unknown", "before_missing", "before_null", "before_alias", "nested_alias", "overflow", "row_id", "agent_mode", "row_shape", "ttl_null", "ttl_fraction", "ttl_exponent", "ttl_huge_exponent", "ttl_tiny_exponent", "ttl_below_minimum", "extra_lf", "no_lf", "raw_hash", "files_conflict", "invocation_conflict", "boot_conflict", "worker_busy", "timer_unknown"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrepareFixture(t)
			name := "before.json"
			raw := append([]byte(nil), f.originals[name]...)
			switch mode {
			case "intent_operation":
				name = "repair.intent.json"
				raw = bytes.Replace(f.originals[name], []byte("session-20261027-first-connection"), []byte("other-operation"), 1)
			case "intent_null":
				name = "repair.intent.json"
				raw = bytes.Replace(f.originals[name], []byte(`"amendment":"credential-rebind-175824179"`), []byte(`"amendment":null`), 1)
			case "intent_unknown":
				name = "repair.intent.json"
				raw = append([]byte(`{"extra":1,`), f.originals[name][1:]...)
			case "before_missing":
				raw = bytes.Replace(raw, []byte(`"app_id":5469697,`), nil, 1)
			case "before_null":
				raw = bytes.Replace(raw, []byte(`"payload":"\u2603fixture-cipher-sensitive"`), []byte(`"payload":null`), 1)
			case "before_alias":
				raw = append([]byte(`{"APP_ID":5469697,`), raw[1:]...)
			case "nested_alias":
				raw = bytes.Replace(raw, []byte(`"repository":{`), []byte(`"repository":{"ID":"ignored",`), 1)
			case "overflow":
				raw = bytes.Replace(raw, []byte(`"app_id":5469697`), []byte(`"app_id":9223372036854775808`), 1)
			case "row_id":
				raw = bytes.Replace(raw, []byte(`"credential_row":[1,`), []byte(`"credential_row":[2,`), 1)
			case "agent_mode":
				raw = bytes.Replace(raw, []byte(`"github",0,"fixture-webhook-row-sensitive"`), []byte(`"github",false,"fixture-webhook-row-sensitive"`), 1)
			case "row_shape":
				raw = bytes.Replace(raw, []byte(`"github",0,"fixture-webhook-row-sensitive"`), []byte(`"github",0`), 1)
			case "ttl_null":
				raw = bytes.Replace(raw, []byte(`"garm_ttl_seconds":86400.5`), []byte(`"garm_ttl_seconds":null`), 1)
			case "ttl_fraction":
				raw = bytes.Replace(raw, []byte(`86400.5`), []byte(`86400.25`), 1)
			case "ttl_exponent":
				raw = bytes.Replace(raw, []byte(`86400.5`), []byte(`8.64005e4`), 1)
			case "ttl_huge_exponent":
				raw = bytes.Replace(raw, []byte(`86400.5`), []byte(`1e99999999999999999999`), 1)
			case "ttl_tiny_exponent":
				raw = bytes.Replace(raw, []byte(`86400.5`), []byte(`1e-99999999999999999999`), 1)
			case "ttl_below_minimum":
				raw = bytes.Replace(raw, []byte(`86400.5`), []byte(`86399.999999999999999999999999`), 1)
			case "extra_lf":
				raw = append(raw, '\n')
			case "no_lf":
				raw = raw[:len(raw)-1]
			case "raw_hash":
				raw = bytes.Replace(raw, []byte(`\u2603`), []byte("☃"), 1)
			case "files_conflict":
				raw = bytes.Replace(raw, []byte(`"size":7`), []byte(`"size":8`), 1)
			case "invocation_conflict":
				name = "services/services-before.json"
				raw = bytes.Replace(f.originals[name], []byte(prepareFixtureInvocation), []byte(strings.Repeat("f", 32)), 1)
			case "boot_conflict":
				name = "services/service-binding.json"
				raw = bytes.Replace(f.originals[name], []byte(prepareFixtureBoot), []byte("ffffffff-ffff-4fff-8fff-ffffffffffff"), 1)
			case "worker_busy":
				name = "services/services-before.json"
				raw = bytes.Replace(f.originals[name], []byte(`"nucleiron-provider-watchdog.service":{"active":"active","sub":"running","job_id":0,"jobs":[]`), []byte(`"nucleiron-provider-watchdog.service":{"active":"activating","sub":"start","job_id":777,"jobs":[{"id":777,"unit":"nucleiron-provider-watchdog.service","path":"/org/freedesktop/systemd1/job/777"}]`), 1)
			case "timer_unknown":
				name = "services/services-before.json"
				raw = bytes.Replace(f.originals[name], []byte(`"nucleiron-provider-watchdog.timer":{`), []byte(`"nucleiron-provider-watchdog.timer":{"main_pid":0,`), 1)
			}
			if bytes.Equal(raw, f.originals[name]) {
				t.Fatal("fixture mutation was not applied")
			}
			testWrite(t, filepath.Join(f.amendment, name), raw, 0600)
			hashOnly := mode == "raw_hash" || mode == "extra_lf" || mode == "no_lf"
			if name == "before.json" && !hashOnly {
				testWrite(t, filepath.Join(f.amendment, "prepare.complete.json"), []byte(fmt.Sprintf("{\"before_sha256\":%q}\n", prepareTestSHA(raw[:len(raw)-1]))), 0600)
			}
			r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
			assertPrepareRestrictions(t, r)
			allowed := mode == "ttl_fraction" || mode == "ttl_exponent" || mode == "worker_busy"
			if (r.State == "prepare_records_consistent") != allowed {
				t.Fatalf("%s: %+v", mode, r)
			}
			if r.Lock.State != "observed" || r.Release.State != "metadata_observed" || r.Publication.State != "observed" {
				t.Fatal("unrelated admission failure masked the mutation", r.Lock, r.Release, r.Publication)
			}
			schemaFailure := -1
			switch mode {
			case "intent_operation", "intent_null", "intent_unknown":
				schemaFailure = 0
			case "timer_unknown":
				schemaFailure = 3
			case "raw_hash", "files_conflict", "invocation_conflict", "boot_conflict", "ttl_fraction", "ttl_exponent", "worker_busy":
			default:
				schemaFailure = 1
			}
			for i, record := range r.Records {
				if record.Presence.State != "observed" {
					t.Fatal("missing input masked schema validation", record)
				}
				if i == schemaFailure {
					if record.Schema.State != "unknown" || record.Schema.Code != "record_schema_or_binding" {
						t.Fatal("intended schema refusal was not observed", record)
					}
				} else if record.Schema.State != "observed" {
					t.Fatal("another schema failure masked the mutation", record)
				}
			}
			if schemaFailure == 1 {
				if r.BeforeHash.State != "unknown" || r.BeforeHash.Code != "before_hash_unavailable" {
					t.Fatal("schema/framing must reject before the byte-seal check", r.BeforeHash)
				}
			} else if mode == "raw_hash" {
				if r.BeforeHash.State != "mismatch" || r.BeforeHash.Code != "before_digest" || r.CrossBinding.State != "observed" {
					t.Fatal("raw-byte mutation was not isolated from schema/binding", r.BeforeHash, r.CrossBinding)
				}
			} else if r.BeforeHash.State != "observed" {
				t.Fatal("byte-seal failure masked the mutation", r.BeforeHash)
			}
			switch mode {
			case "files_conflict", "boot_conflict":
				if r.CrossBinding.State != "mismatch" || r.CrossBinding.Code != "historical_binding_conflict" {
					t.Fatal("intended cross-binding refusal was not observed", r.CrossBinding)
				}
			case "invocation_conflict":
				if r.CrossBinding.State != "mismatch" || r.CrossBinding.Code != "historical_invocation_conflict" {
					t.Fatal("historical sampling conflict lost", r.CrossBinding)
				}
			default:
				if allowed && r.CrossBinding.State != "observed" {
					t.Fatal("valid mutation lost its cross-binding", r.CrossBinding)
				}
			}
		})
	}
}

func TestPrepareUnsafeFilesPendingAndExtraEntries(t *testing.T) {
	for _, mode := range []string{"mode", "hardlink", "target_pending_link", "pending", "pending_symlink", "pending_fifo", "fifo", "symlink", "oversize", "lock_missing", "lock_busy", "lock_link", "lock_nonempty", "owner", "other_entry", "services_unsafe", "services_missing"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrepareFixture(t)
			path := filepath.Join(f.amendment, "before.json")
			switch mode {
			case "mode":
				if e := os.Chmod(path, 0644); e != nil {
					t.Fatal(e)
				}
			case "hardlink":
				if e := os.Link(path, filepath.Join(f.reader.parent, "retained-before")); e != nil {
					t.Fatal(e)
				}
			case "target_pending_link":
				if e := os.Link(path, path+".pending"); e != nil {
					t.Fatal(e)
				}
			case "pending":
				testWrite(t, path+".pending", []byte("partial-sensitive"), 0600)
			case "pending_symlink":
				if e := os.Symlink("missing", path+".pending"); e != nil {
					t.Fatal(e)
				}
			case "pending_fifo":
				if e := unix.Mkfifo(path+".pending", 0600); e != nil {
					t.Fatal(e)
				}
			case "services_unsafe":
				if e := os.Chmod(filepath.Join(f.amendment, "services"), 0000); e != nil {
					t.Fatal(e)
				}
				defer os.Chmod(filepath.Join(f.amendment, "services"), 0700)
			case "services_missing":
				if e := os.RemoveAll(filepath.Join(f.amendment, "services")); e != nil {
					t.Fatal(e)
				}
			case "fifo":
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
				if e := unix.Mkfifo(path, 0600); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				if e := os.Rename(path, path+"-retained"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(path+"-retained", path); e != nil {
					t.Fatal(e)
				}
			case "oversize":
				file, e := os.OpenFile(path, os.O_WRONLY, 0)
				if e != nil {
					t.Fatal(e)
				}
				e = file.Truncate(16*1024*1024 + 1)
				file.Close()
				if e != nil {
					t.Fatal(e)
				}
			case "lock_missing":
				if e := os.Remove(filepath.Join(f.reader.parent, "owner.lock")); e != nil {
					t.Fatal(e)
				}
			case "lock_busy":
				file, e := os.OpenFile(filepath.Join(f.reader.parent, "owner.lock"), os.O_RDONLY, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer file.Close()
				if unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
					t.Fatal("fixture lock")
				}
			case "lock_link":
				if e := os.Link(filepath.Join(f.reader.parent, "owner.lock"), filepath.Join(f.reader.parent, "retained-lock")); e != nil {
					t.Fatal(e)
				}
			case "lock_nonempty":
				testWrite(t, filepath.Join(f.reader.parent, "owner.lock"), []byte("old"), 0600)
			case "owner":
				f.reader.files.uid++
			case "other_entry":
				if e := unix.Mkfifo(filepath.Join(f.amendment, "garm-login.result.json"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
			assertPrepareRestrictions(t, r)
			if r.State == "prepare_records_consistent" {
				t.Fatal("unsafe/partial material accepted", mode)
			}
			if mode == "lock_missing" {
				if _, e := os.Lstat(filepath.Join(f.reader.parent, "owner.lock")); !os.IsNotExist(e) {
					t.Fatal("created original lock")
				}
			}
			if mode == "target_pending_link" || mode == "pending" || mode == "pending_symlink" || mode == "pending_fifo" {
				if r.Publication.Code != "publication_incomplete" {
					t.Fatal("lost interrupted publication", r.Publication)
				}
				if len(r.Pending) != 1 || mode == "target_pending_link" && r.Pending[0].State != "linked_publication_incomplete" {
					t.Fatal("pending object metadata was lost", r.Pending)
				}
			}
			if mode == "services_unsafe" && (r.ServicesDirectory.State != "unknown" || r.Records[2].Presence.State != "unknown" || r.Records[3].Presence.State != "unknown") {
				t.Fatal("unreadable became absence", r.ServicesDirectory, r.Records)
			}
			if mode == "services_missing" && (r.ServicesDirectory.State != "not_observed" || r.Records[2].Presence.State != "not_observed") {
				t.Fatal("missing directory classification lost", r.ServicesDirectory, r.Records)
			}
		})
	}
}

func TestPreparePinnedObservationRejectsSynchronizedChanges(t *testing.T) {
	for _, mode := range []string{"record_bytes", "same_bytes_new_object", "amendment_directory", "release_directory", "manifest_bytes", "staging_member", "lock_object", "pending_appeared", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrepareFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			entered := false
			f.reader.beforeRecheck = func() {
				entered = true
				if !heldFixtureLock(filepath.Join(f.reader.parent, "owner.lock")) {
					t.Fatal("released original lock before final check")
				}
				switch mode {
				case "record_bytes":
					testWrite(t, filepath.Join(f.amendment, "before.json"), []byte("changed-sensitive"), 0600)
				case "same_bytes_new_object":
					path := filepath.Join(f.amendment, "before.json")
					if e := os.Rename(path, path+"-retained"); e != nil {
						t.Fatal(e)
					}
					testWrite(t, path, f.originals["before.json"], 0600)
				case "amendment_directory":
					if e := os.Rename(f.amendment, f.amendment+"-retained"); e != nil {
						t.Fatal(e)
					}
					if e := os.Mkdir(f.amendment, 0700); e != nil {
						t.Fatal(e)
					}
				case "release_directory":
					if e := os.Rename(f.reader.release, f.reader.release+"-retained"); e != nil {
						t.Fatal(e)
					}
					if e := os.Mkdir(f.reader.release, 0700); e != nil {
						t.Fatal(e)
					}
				case "manifest_bytes":
					testWrite(t, filepath.Join(f.reader.release, "runtime-manifest.json"), []byte("changed"), 0600)
				case "staging_member":
					testWrite(t, filepath.Join(f.reader.release, "entry.py"), []byte("new"), 0600)
				case "lock_object":
					path := filepath.Join(f.reader.parent, "owner.lock")
					if e := os.Rename(path, path+"-retained"); e != nil {
						t.Fatal(e)
					}
					testWrite(t, path, nil, 0600)
				case "pending_appeared":
					testWrite(t, filepath.Join(f.amendment, "before.json.pending"), []byte("partial"), 0600)
				case "cancel":
					cancel()
				}
			}
			r := f.reader.observe(ctx, strings.Repeat("9", 64))
			assertPrepareRestrictions(t, r)
			if !entered || r.State == "prepare_records_consistent" || r.Historical != nil {
				t.Fatalf("changed observation escaped: %+v", r)
			}
		})
	}
}

func TestPrepareNoAtimeAndNoWrites(t *testing.T) {
	f := newPrepareFixture(t)
	probe := newPrepareFiles(f.reader.files)
	defer probe.close()
	if _, err := probe.open(f.reader.release, true, 0700, false); err != nil {
		var st unix.Stat_t
		unix.Stat(f.reader.release, &st)
		t.Fatalf("synthetic filesystem admission: err=%v uid=%d mode=%#o expected_uid=%d parents=%v", err, st.Uid, st.Mode, f.reader.files.uid, f.reader.files.parents(f.reader.release))
	}
	old := time.Unix(946684800, 0)
	paths := []string{}
	if e := filepath.WalkDir(f.reader.files.boundary, func(p string, _ os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		paths = append(paths, p)
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	stats := map[string]unix.Stat_t{}
	for _, p := range paths {
		if e := os.Chtimes(p, old, old); e != nil {
			t.Fatal(e)
		}
		var st unix.Stat_t
		if unix.Stat(p, &st) != nil {
			t.Fatal("stat fixture")
		}
		stats[p] = st
	}
	r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
	if r.State != "prepare_records_consistent" {
		t.Fatalf("critical O_NOATIME path: %+v", r)
	}
	for p, before := range stats {
		var after unix.Stat_t
		if unix.Stat(p, &after) != nil || !samePrepareStat(before, after) {
			t.Fatal("reader changed original metadata", filepath.Base(p))
		}
	}
}

func TestPrepareReleaseStagingIndependentAndUnsupportedPins(t *testing.T) {
	for _, mode := range []string{"member_missing", "member_partial", "member_link", "adapters_missing", "manifest_missing", "manifest_null", "manifest_duplicate", "manifest_path_escape", "plan_digest", "plan_null", "plan_identity", "plan_missing", "unknown_entry", "missing_amendment", "missing_release"} {
		t.Run(mode, func(t *testing.T) {
			f := newPrepareFixture(t)
			switch mode {
			case "member_missing":
				if e := os.Remove(filepath.Join(f.reader.release, "credential")); e != nil {
					t.Fatal(e)
				}
			case "member_partial":
				testWrite(t, filepath.Join(f.reader.release, "entry.py"), []byte("x"), 0600)
			case "member_link":
				path := filepath.Join(f.reader.release, "entry.py")
				if e := os.Link(path, filepath.Join(f.reader.files.boundary, "retained-member")); e != nil {
					t.Fatal(e)
				}
			case "adapters_missing":
				if e := os.RemoveAll(filepath.Join(f.reader.release, "adapters")); e != nil {
					t.Fatal(e)
				}
			case "manifest_missing":
				if e := os.Remove(filepath.Join(f.reader.release, "runtime-manifest.json")); e != nil {
					t.Fatal(e)
				}
			case "manifest_null":
				testWrite(t, filepath.Join(f.reader.release, "runtime-manifest.json"), []byte("null"), 0600)
				f.reader.releaseSHA = prepareTestSHA([]byte("null"))
			case "manifest_duplicate", "manifest_path_escape":
				path := filepath.Join(f.reader.release, "runtime-manifest.json")
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				replacement := []byte(`"host.py"`)
				if mode == "manifest_path_escape" {
					replacement = []byte(`"../../outside-sensitive"`)
				}
				raw = bytes.Replace(raw, []byte(`"entry.py"`), replacement, 1)
				testWrite(t, path, raw, 0600)
				f.reader.releaseSHA = prepareTestSHA(raw)
			case "plan_digest":
				f.reader.planSHA = strings.Repeat("f", 64)
			case "plan_null", "plan_identity", "plan_missing":
				path := filepath.Join(f.reader.release, "host-plan.json")
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				switch mode {
				case "plan_null":
					raw = []byte("null")
				case "plan_identity":
					raw = bytes.Replace(raw, []byte(`"repository_id":1432619931`), []byte(`"repository_id":1432619932`), 1)
				case "plan_missing":
					raw = bytes.Replace(raw, []byte(`"expires_at":1793118600,`), nil, 1)
				}
				testWrite(t, path, raw, 0600)
				f.reader.planSHA = prepareTestSHA(raw)
			case "unknown_entry":
				testWrite(t, filepath.Join(f.reader.release, "unexpected-sensitive-name"), []byte("must-not-read-sensitive"), 0600)
			case "missing_amendment":
				if e := os.RemoveAll(f.amendment); e != nil {
					t.Fatal(e)
				}
			case "missing_release":
				if e := os.RemoveAll(f.reader.release); e != nil {
					t.Fatal(e)
				}
			}
			r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
			assertPrepareRestrictions(t, r)
			if mode == "missing_amendment" {
				if r.AmendmentDirectory.State != "not_observed" || r.Release.State != "metadata_observed" {
					t.Fatal("lock-before-stage distinction lost", r)
				}
			} else if r.Release.State == "metadata_observed" {
				t.Fatal("incomplete staging reported whole", r.Release)
			}
			switch mode {
			case "manifest_null", "manifest_duplicate", "manifest_path_escape":
				if r.Release.Manifest.State != "unknown" || r.Release.Manifest.Code != "manifest_schema" {
					t.Fatal("manifest digest failure masked the intended schema case", r.Release.Manifest)
				}
			case "plan_null", "plan_identity", "plan_missing":
				if r.Release.Plan.State != "unknown" || r.Release.Plan.Code != "plan_schema" {
					t.Fatal("plan digest failure masked the intended schema case", r.Release.Plan)
				}
			case "plan_digest":
				if r.Release.Plan.State != "mismatch" || r.Release.Plan.Code != "plan_digest" {
					t.Fatal("digest-specific refusal was not observed", r.Release.Plan)
				}
			case "member_link":
				if r.Release.OtherEntries != 0 || r.Release.Manifest.State != "observed" || r.Release.Plan.State != "observed" || r.Release.Adapters.State != "observed" {
					t.Fatal("another staging refusal masked the hardlink", r.Release)
				}
				for _, member := range r.Release.Members {
					want := "metadata_observed"
					if member.Name == "entry.py" {
						want = "unknown"
					}
					if member.State != want {
						t.Fatal("hardlink member was not isolated", member)
					}
				}
			}
		})
	}
}

func TestPrepareStagedMemberContentIsNotObserved(t *testing.T) {
	f := newPrepareFixture(t)
	// The fixed manifest remains byte-for-byte unchanged. A different body of
	// the same size must never be described as a verified content hash.
	testWrite(t, filepath.Join(f.reader.release, "entry.py"), []byte("new"), 0600)
	r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
	assertPrepareRestrictions(t, r)
	if r.Release.Manifest.State != "observed" || r.Release.State != "metadata_observed" || r.Release.Content != "not_observed_this_slice" {
		t.Fatal("metadata was upgraded to content evidence", r.Release)
	}
	found := false
	for _, member := range r.Release.Members {
		if member.Name == "entry.py" {
			found = true
			if member.State != "metadata_observed" {
				t.Fatal(member)
			}
		}
	}
	if !found {
		t.Fatal("fixed member observation omitted")
	}
}

func TestPrepareWorkerMayExitBetweenHistoricalSamples(t *testing.T) {
	f := newPrepareFixture(t)
	// Independent original-format values: Unit was still active/running;
	// later Service/GetUnitProcesses observations saw the worker's exit.
	const before = `"nucleiron-provider-watchdog.service":{"active":"active","sub":"running","job_id":0,"jobs":[],"invocation":"63717a25032d4e048d801cbf935a18dc","main_pid":120,"control_pid":0,"pids":[120],"result":"success","restarts":0,"start_us":12345678901234567,"exit_us":0,"exit_status":0,"exit_code":0}`
	const after = `"nucleiron-provider-watchdog.service":{"active":"active","sub":"running","job_id":0,"jobs":[],"invocation":"63717a25032d4e048d801cbf935a18dc","main_pid":0,"control_pid":0,"pids":[],"result":"success","restarts":0,"start_us":12345678901234567,"exit_us":12345678901234568,"exit_status":0,"exit_code":1}`
	name := "services/services-before.json"
	raw := bytes.Replace(f.originals[name], []byte(before), []byte(after), 1)
	if bytes.Equal(raw, f.originals[name]) {
		t.Fatal("historical transition fixture was not applied")
	}
	testWrite(t, filepath.Join(f.amendment, name), raw, 0600)
	r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
	assertPrepareRestrictions(t, r)
	if r.State != "prepare_records_consistent" || r.Records[3].Schema.State != "observed" || r.BeforeHash.State != "observed" || r.CrossBinding.State != "observed" || r.Historical == nil {
		t.Fatalf("legitimate historical transition rejected: %+v", r)
	}
	worker := r.Historical.Services["nucleiron-provider-watchdog.service"]
	if worker.Active != "active" || worker.Main != 0 || worker.Control != 0 || worker.PIDs != 0 || worker.Invocation != "63717a25032d4e048d801cbf935a18dc" {
		t.Fatal("historical facts were rewritten into an atomic state", worker)
	}
}

func TestPrepareEntrypointFixedAndRedacted(t *testing.T) {
	for _, args := range [][]string{{"inspect-prepare", "--evidence-dir", "/fixture-sensitive"}, {"inspect-prepare", "--release-sha256", strings.Repeat("f", 64), "--plan-sha256", strings.Repeat("a", 64)}, {"inspect-prepare", "--token-file", "fixture-sensitive"}} {
		var output, diagnostic bytes.Buffer
		if Run(context.Background(), args, &output, &diagnostic) != 1 || output.Len() != 0 || diagnostic.String() != "{\"event\":\"prepare_observation_refused\",\"stage\":\"arguments\"}\n" {
			t.Fatal("CLI escaped fixed bounds", output.String(), diagnostic.String())
		}
	}
}

func TestPrepareConcurrentObserverAndInventoryBound(t *testing.T) {
	t.Run("original_lock_lifetime", func(t *testing.T) {
		f := newPrepareFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		first := f.reader
		first.beforeRecheck = func() {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		done := make(chan prepareReport, 1)
		go func() { done <- first.observe(ctx, strings.Repeat("9", 64)) }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("first observer did not reach locked final check")
		}
		second := f.reader.observe(ctx, strings.Repeat("9", 64))
		assertPrepareRestrictions(t, second)
		if second.Lock.Code != "owner_lock_busy" || second.State == "prepare_records_consistent" {
			t.Fatal("second observer bypassed original lock", second.Lock)
		}
		close(release)
		r := <-done
		assertPrepareRestrictions(t, r)
		if r.State != "prepare_records_consistent" {
			t.Fatal("second read disturbed original snapshot", r)
		}
	})
	t.Run("bounded_complete_inventory", func(t *testing.T) {
		f := newPrepareFixture(t)
		for i := 0; i < 4097; i++ {
			file, e := os.OpenFile(filepath.Join(f.reader.release, fmt.Sprintf("unexpected-%04d", i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				t.Fatal(e)
			}
			file.Close()
		}
		r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
		assertPrepareRestrictions(t, r)
		if r.Release.State != "unknown" || r.State == "prepare_records_consistent" {
			t.Fatal("truncated inventory became complete", r.Release)
		}
	})
}
