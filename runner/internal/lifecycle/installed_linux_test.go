package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type installedFixture struct {
	reader installedReader
	plan   []byte
	paths  []string
}

func newInstalledFixture(t *testing.T) installedFixture {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("exact root-owned historical plan requires the private root fixture container")
	}
	f := newPrepareFixture(t)
	root := filepath.Join(f.reader.files.boundary, "installed")
	// Independent fixture paths and original-format fact rows are shared with
	// the historical fixture, not derived from the installed observer's result.
	facts := map[string]json.RawMessage{}
	if json.Unmarshal([]byte(syntheticPrepareFacts()), &facts) != nil {
		t.Fatal("fixture facts")
	}
	paths := []string{}
	for path := range facts {
		paths = append(paths, path)
	}
	rows := map[string]any{}
	for _, path := range paths {
		body := []byte("synthetic-private-key-not-a-real-secret:" + path)
		testWrite(t, filepath.Join(root, path), body, 0600)
		rows[path] = map[string]any{"sha256": prepareTestSHA(body), "size": len(body), "uid": 0, "gid": os.Getgid(), "mode": 384, "xattrs_sha256": "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"}
	}
	planPath := filepath.Join(f.reader.release, "host-plan.json")
	raw, e := os.ReadFile(planPath)
	if e != nil {
		t.Fatal(e)
	}
	var plan map[string]json.RawMessage
	if json.Unmarshal(raw, &plan) != nil {
		t.Fatal("fixture plan")
	}
	plan["fixed_files"], e = json.Marshal(rows)
	if e != nil {
		t.Fatal(e)
	}
	raw, e = json.Marshal(plan)
	if e != nil {
		t.Fatal(e)
	}
	testWrite(t, planPath, raw, 0600)
	return installedFixture{installedReader{files: f.reader.files, parent: f.reader.parent, release: f.reader.release, planSHA: prepareTestSHA(raw), root: root}, raw, paths}
}

func assertInstalledRestrictions(t *testing.T, r installedReport) {
	t.Helper()
	if r.OperationComplete || r.Recovered || r.Retry || r.Enable || r.ServiceWrite || r.Cleanup || r.Migration || r.VMAcceptance || !reflect.DeepEqual(r.Gaps, []string{"historical_producer_execution_unproven", "prepare_records_not_observed", "current_services_boot_api_database_resources_not_observed", "additional_watchdog_not_observed", "full_host_facts_not_observed", "credentials_not_decrypted_or_used"}) {
		t.Fatal("observation acquired authority", r)
	}
	raw, e := json.Marshal(r)
	if e != nil || bytes.Contains(raw, []byte("synthetic-private-key")) || bytes.Contains(raw, []byte("fixture-")) || bytes.Contains(raw, []byte(prepareTestSHA([]byte("synthetic-private-key-not-a-real-secret:/usr/sbin/nginx")))) {
		t.Fatal("sensitive fact escaped")
	}
}

func installedResult(t *testing.T, r installedReport, path string) prepareStatus {
	t.Helper()
	for _, v := range r.Files {
		if v.Path == path {
			return v.prepareStatus
		}
	}
	t.Fatal("missing fixed path", path)
	return prepareStatus{}
}

func TestInstalledHistoricalMatchAndReadOnlyReentry(t *testing.T) {
	f := newInstalledFixture(t)
	stats := map[string]unix.Stat_t{}
	for _, path := range f.paths {
		actual := filepath.Join(f.reader.root, path)
		old := time.Unix(10000, 0)
		if os.Chtimes(actual, old, old) != nil {
			t.Fatal("fixture times")
		}
		var st unix.Stat_t
		if unix.Stat(actual, &st) != nil {
			t.Fatal("fixture stat")
		}
		stats[actual] = st
	}
	for i := 0; i < 2; i++ {
		r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
		assertInstalledRestrictions(t, r)
		if r.State != "installed_files_match_historical_plan" || len(r.Files) != 25 || r.Lock.State != "observed" || r.Plan.State != "observed" || r.PlanExpires == nil || *r.PlanExpires != 1793118600 {
			t.Fatal("independent complete expectation", r)
		}
		for _, v := range r.Files {
			if v.State != "matched" || v.Code != "" {
				t.Fatal(v)
			}
		}
	}
	for path, before := range stats {
		var after unix.Stat_t
		if unix.Stat(path, &after) != nil || !samePrepareStat(before, after) {
			t.Fatal("read changed metadata", path)
		}
	}
	raw, e := os.ReadFile(filepath.Join(f.reader.release, "host-plan.json"))
	if e != nil || !bytes.Equal(raw, f.plan) {
		t.Fatal("plan changed")
	}
}

func TestInstalledFileStates(t *testing.T) {
	for _, mode := range []string{"content", "size", "mode", "special_mode", "gid", "uid", "missing", "symlink", "hardlink", "fifo", "parent", "xattr"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstalledFixture(t)
			name := "/usr/sbin/nginx"
			path := filepath.Join(f.reader.root, name)
			want := prepareStatus{"mismatch", "file_" + mode}
			switch mode {
			case "content":
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				raw[0] = 'X'
				testWrite(t, path, raw, 0600)
			case "size":
				testWrite(t, path, []byte("short"), 0600)
			case "mode":
				if os.Chmod(path, 0644) != nil {
					t.Fatal("chmod")
				}
			case "special_mode":
				if unix.Chmod(path, 04600) != nil {
					t.Fatal("special chmod")
				}
				want.Code = "file_mode"
			case "gid":
				if os.Chown(path, 0, 12345) != nil {
					t.Fatal("chown")
				}
			case "uid":
				if os.Chown(path, 12345, os.Getgid()) != nil {
					t.Fatal("chown")
				}
			case "missing":
				if os.Remove(path) != nil {
					t.Fatal("remove")
				}
				want = prepareStatus{"missing", "file_missing"}
			case "symlink":
				if os.Remove(path) != nil || os.Symlink("fixture-sensitive-target", path) != nil {
					t.Fatal("symlink")
				}
				// O_PATH|O_NOFOLLOW may observe the final symlink itself; it
				// must remain an unsafe object, never a missing target.
				want = prepareUnknown("file_unsafe")
			case "hardlink":
				if os.Link(path, filepath.Join(f.reader.root, "retained")) != nil {
					t.Fatal("link")
				}
				want = prepareUnknown("file_unsafe")
			case "fifo":
				if os.Remove(path) != nil || unix.Mkfifo(path, 0600) != nil {
					t.Fatal("fifo")
				}
				want = prepareUnknown("file_unsafe")
			case "parent":
				if os.Chmod(filepath.Dir(path), 0777) != nil {
					t.Fatal("parent mode")
				}
				want = prepareUnknown("parent_untrusted")
			case "xattr":
				if unix.Setxattr(path, "user.synthetic", []byte("private-attribute-value"), 0) != nil {
					t.Fatal("xattr unsupported")
				}
				want.Code = "file_xattrs"
			}
			r := f.reader.observe(context.Background(), strings.Repeat("9", 64))
			assertInstalledRestrictions(t, r)
			if got := installedResult(t, r, name); got != want {
				t.Fatal("wrong file classification", got, want)
			}
			if r.State != "unknown" || r.Plan.State != "observed" || installedResult(t, r, "/etc/nucleiron-egress/dns.yaml").State != "matched" {
				t.Fatal("partial observations hidden", r)
			}
		})
	}
}

func TestInstalledInputsBlockFileReads(t *testing.T) {
	for _, mode := range []string{"lock_missing", "lock_busy", "lock_link", "lock_mode", "plan_missing", "plan_digest", "plan_null", "plan_extra", "plan_xattrs", "producer"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstalledFixture(t)
			planPath := filepath.Join(f.reader.release, "host-plan.json")
			lockPath := filepath.Join(f.reader.parent, "owner.lock")
			producer := strings.Repeat("9", 64)
			switch mode {
			case "lock_missing":
				if os.Remove(lockPath) != nil {
					t.Fatal("remove lock")
				}
			case "lock_busy":
				lock, e := openPath(lockPath, unix.O_RDONLY, 0)
				if e != nil {
					t.Fatal(e)
				}
				defer lock.Close()
				if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
					t.Fatal("lock fixture")
				}
			case "lock_link":
				if os.Link(lockPath, filepath.Join(f.reader.parent, "retained-lock")) != nil {
					t.Fatal("link lock")
				}
			case "lock_mode":
				if os.Chmod(lockPath, 0644) != nil {
					t.Fatal("mode lock")
				}
			case "plan_missing":
				if os.Remove(planPath) != nil {
					t.Fatal("remove plan")
				}
			case "plan_digest":
				f.reader.planSHA = strings.Repeat("f", 64)
			case "producer":
				producer = "invalid-sensitive"
			default:
				raw := f.plan
				if mode == "plan_null" {
					raw = []byte("null")
				} else if mode == "plan_extra" {
					raw = bytes.Replace(raw, []byte(`"fixed_files":{`), []byte(`"fixed_files":{"/unexpected-sensitive":{},`), 1)
				} else {
					raw = bytes.ReplaceAll(raw, []byte(installedEmptyXattrs), []byte(strings.Repeat("a", 64)))
				}
				if bytes.Equal(raw, f.plan) {
					t.Fatal("mutation not applied")
				}
				testWrite(t, planPath, raw, 0600)
				f.reader.planSHA = prepareTestSHA(raw)
			}
			f.reader.beforeBody = func(string) { t.Fatal("invalid input reached installed body") }
			r := f.reader.observe(context.Background(), producer)
			assertInstalledRestrictions(t, r)
			if r.State != "unknown" {
				t.Fatal(r)
			}
			switch mode {
			case "lock_busy":
				if r.Lock != prepareUnknown("owner_lock_busy") {
					t.Fatal("lock conflict hidden", r.Lock)
				}
			case "plan_digest":
				if r.Plan != (prepareStatus{"mismatch", "plan_digest"}) {
					t.Fatal("digest rejection hidden", r.Plan)
				}
			case "plan_null", "plan_extra":
				if r.Plan != prepareUnknown("plan_schema") {
					t.Fatal("schema rejection masked by digest", r.Plan)
				}
			case "plan_xattrs":
				if r.Plan != prepareUnknown("plan_xattrs_unsupported") {
					t.Fatal("unsupported contract hidden", r.Plan)
				}
			case "producer":
				if r.ObserverSHA != "" || r.Plan != prepareUnknown("observer_producer_unavailable") {
					t.Fatal("invalid observer identity accepted", r.Plan)
				}
			default:
				if mode == "plan_missing" {
					if r.Plan != prepareUnknown("plan_unavailable") {
						t.Fatal(r.Plan)
					}
				} else if r.Lock != prepareUnknown("owner_lock_unavailable") {
					t.Fatal(r.Lock)
				}
			}
			for _, file := range r.Files {
				if file.State != "unknown" || file.Code != "not_read" {
					t.Fatal("input failure did not block observation", file)
				}
			}
		})
	}
}

func TestInstalledConcurrentChangesAndCancellation(t *testing.T) {
	for _, mode := range []string{"replace", "same_size", "appear", "xattr", "parent_replace", "plan_change", "lock_replace", "cancel", "fifo_open_race"} {
		t.Run(mode, func(t *testing.T) {
			f := newInstalledFixture(t)
			name := "/usr/sbin/nginx"
			path := filepath.Join(f.reader.root, name)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "appear" && os.Remove(path) != nil {
				t.Fatal("remove fixture")
			}
			mutate := func() {
				switch mode {
				case "replace":
					if os.Rename(path, path+".retained") != nil {
						t.Fatal("rename")
					}
					testWrite(t, path, []byte("new"), 0600)
				case "same_size":
					raw, e := os.ReadFile(path)
					if e != nil {
						t.Fatal(e)
					}
					raw[0] = 'X'
					testWrite(t, path, raw, 0600)
				case "appear":
					testWrite(t, path, []byte("new"), 0600)
				case "xattr":
					if unix.Setxattr(path, "user.synthetic", []byte("sensitive"), 0) != nil {
						t.Fatal("xattr")
					}
				case "parent_replace":
					dir := filepath.Dir(path)
					if os.Rename(dir, dir+".retained") != nil || os.Mkdir(dir, 0700) != nil || os.Link(filepath.Join(dir+".retained", "nginx"), path) != nil || os.Remove(filepath.Join(dir+".retained", "nginx")) != nil {
						t.Fatal("replace parent with same leaf")
					}
				case "plan_change":
					testWrite(t, filepath.Join(f.reader.release, "host-plan.json"), []byte("changed"), 0600)
				case "lock_replace":
					lock := filepath.Join(f.reader.parent, "owner.lock")
					if os.Rename(lock, lock+".retained") != nil {
						t.Fatal("rename lock")
					}
					testWrite(t, lock, nil, 0600)
				case "cancel":
					cancel()
				case "fifo_open_race":
					if os.Remove(path) != nil || unix.Mkfifo(path, 0600) != nil {
						t.Fatal("replace fifo")
					}
				}
			}
			if mode == "fifo_open_race" {
				f.reader.beforeBody = func(p string) {
					if p == name {
						mutate()
					}
				}
			} else {
				f.reader.beforeRecheck = mutate
			}
			r := f.reader.observe(ctx, strings.Repeat("9", 64))
			assertInstalledRestrictions(t, r)
			if r.State != "unknown" {
				t.Fatal("concurrent change accepted", r)
			}
			if mode == "plan_change" || mode == "lock_replace" {
				if r.Plan.Code != "inputs_changed" || r.Lock.Code != "inputs_changed" {
					t.Fatal("inputs change hidden", r)
				}
			} else if got := installedResult(t, r, name); got.State != "unknown" {
				t.Fatal("changed leaf kept prior classification", got)
			}
		})
	}
}

func TestInstalledStreamingBoundsAndPrivateFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private")
	testWrite(t, path, []byte("sensitive"), 0600)
	f, e := openPath(path, unix.O_RDONLY|unix.O_NOATIME, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e := installedHash(context.Background(), f, 8); e == nil {
		t.Fatal("growth accepted")
	}
	if _, e := installedHash(context.Background(), f, 10); e == nil {
		t.Fatal("truncation accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := installedHash(ctx, f, 9); e == nil {
		t.Fatal("cancelled read accepted")
	}
	got, e := installedHash(context.Background(), f, 9)
	if e != nil || got != prepareTestSHA([]byte("sensitive")) {
		t.Fatal("independent hash expectation", e)
	}
	s := installedFiles{protection: protectedFiles{uid: uint32(os.Getuid()), boundary: root}, parents: map[string]*installedObject{}}
	defer s.close()
	uid, gid, mode, size, sha, attrs := uint32(os.Getuid()), uint32(os.Getgid()), uint32(0600), int64(9), prepareTestSHA([]byte("sensitive")), installedEmptyXattrs
	if got := s.observe(context.Background(), path, prepareFact{&sha, &size, &uid, &gid, &mode, &attrs}, nil); got.State != "matched" {
		var st unix.Stat_t
		_ = unix.Stat(root, &st)
		t.Fatal(got, "fixture parent uid/mode", st.Uid, st.Mode, "process uid", os.Getuid(), "parent check", s.protection.parents(path))
	}
	if !s.unchanged(context.Background(), s.objects[0]) {
		t.Fatal("stable actual descriptor rejected")
	}
	if os.Getuid() != 0 {
		if os.Chmod(path, 0000) != nil {
			t.Fatal("fixture permission")
		}
		defer os.Chmod(path, 0600)
		mode = 0
		if got := s.observe(context.Background(), path, prepareFact{&sha, &size, &uid, &gid, &mode, &attrs}, nil); got != prepareUnknown("file_body_unavailable") {
			t.Fatal("permission denial became absence", got)
		}
	}
}

type installedFailWriter struct{ short bool }

func (w installedFailWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("sensitive output error")
}

func TestInstalledCLIAndOutputBoundary(t *testing.T) {
	for _, args := range [][]string{{"--root", "sensitive"}, {"--plan-sha256", strings.Repeat("f", 64)}, {"--token-file", "sensitive"}} {
		var out, diag bytes.Buffer
		if code := Run(context.Background(), append([]string{"inspect-installed"}, args...), &out, &diag); code != 1 || out.Len() != 0 || diag.String() != "{\"event\":\"installed_observation_refused\",\"stage\":\"arguments\"}\n" {
			t.Fatal("uncontrolled CLI input accepted", code, &diag)
		}
	}
	for _, writer := range []io.Writer{installedFailWriter{}, installedFailWriter{true}} {
		if writeInstalledReport(writer, installedReport{}) == nil {
			t.Fatal("output failure accepted")
		}
	}
	var out bytes.Buffer
	if writeInstalledReport(&out, installedReport{Version: 1, State: "unknown"}) != nil || !strings.HasSuffix(out.String(), "\n") {
		t.Fatal("fixed output framing")
	}
	if os.Getuid() != 0 {
		var diag bytes.Buffer
		out.Reset()
		if runInstalled(context.Background(), []string{"--release-sha256", prepareReleaseSHA, "--plan-sha256", preparePlanSHA}, &out, &diag) != 1 || !strings.Contains(diag.String(), `"stage":"identity"`) {
			t.Fatal("nonroot entered protected observer")
		}
	}
}
