package lifecycle

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const installedEmptyXattrs = "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

type installedFileStatus struct {
	Path string `json:"path"`
	prepareStatus
}
type installedReport struct {
	Version           int                   `json:"version"`
	Operation         string                `json:"operation"`
	Amendment         string                `json:"amendment"`
	ReleaseSHA        string                `json:"interpretation_manifest_sha256"`
	PlanSHA           string                `json:"interpretation_plan_sha256"`
	ObserverSHA       string                `json:"observer_sha256"`
	ObservedAt        string                `json:"observed_at"`
	State             string                `json:"state"`
	Lock              prepareStatus         `json:"lock"`
	Plan              prepareStatus         `json:"plan"`
	PlanExpires       *int64                `json:"historical_plan_expires_at"`
	Files             []installedFileStatus `json:"files"`
	Gaps              []string              `json:"provenance_gaps"`
	OperationComplete bool                  `json:"operation_complete"`
	Recovered         bool                  `json:"recovered"`
	Retry             bool                  `json:"retry_allowed"`
	Enable            bool                  `json:"enable_allowed"`
	ServiceWrite      bool                  `json:"service_write_allowed"`
	Cleanup           bool                  `json:"cleanup_allowed"`
	Migration         bool                  `json:"migration_allowed"`
	VMAcceptance      bool                  `json:"vm_acceptance"`
}
type installedReader struct {
	files                    protectedFiles
	parent, release, planSHA string
	root                     string // / in production; private synthetic tree in tests only.
	beforeRecheck            func()
	beforeBody               func(string)
}

func (p installedReader) observe(ctx context.Context, producer string) installedReport {
	out := installedReport{Version: 1, Operation: prepareOperation, Amendment: prepareAmendment, ReleaseSHA: prepareReleaseSHA, PlanSHA: p.planSHA, ObserverSHA: producer, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), State: "unknown", Lock: prepareUnknown("owner_lock_unavailable"), Plan: prepareUnknown("plan_unavailable"), Gaps: []string{"historical_producer_execution_unproven", "prepare_records_not_observed", "current_services_boot_api_database_resources_not_observed", "additional_watchdog_not_observed", "full_host_facts_not_observed", "credentials_not_decrypted_or_used"}}
	for _, path := range prepareFixedPaths {
		out.Files = append(out.Files, installedFileStatus{path, prepareUnknown("not_read")})
	}
	if !digestPattern.MatchString(producer) {
		out.ObserverSHA = ""
		out.Plan = prepareUnknown("observer_producer_unavailable")
		return out
	}
	inputs := newPrepareFiles(p.files)
	defer inputs.close()
	files := installedFiles{protection: p.files, parents: map[string]*installedObject{}}
	defer files.close()
	finish := func() installedReport {
		if p.beforeRecheck != nil {
			p.beforeRecheck()
		}
		for _, obj := range files.objects {
			if !files.unchanged(ctx, obj) {
				for i := range out.Files {
					if filepath.Join(p.root, out.Files[i].Path) == obj.path {
						out.Files[i].prepareStatus = prepareUnknown("file_changed")
					}
				}
			}
		}
		if !inputs.unchanged(ctx) {
			out.Lock = prepareUnknown("inputs_changed")
			out.Plan = prepareUnknown("inputs_changed")
		}
		if ctx.Err() != nil {
			out.Plan = prepareUnknown("observation_cancelled")
			for i := range out.Files {
				out.Files[i].prepareStatus = prepareUnknown("observation_cancelled")
			}
		}
		all := out.Lock.State == "observed" && out.Plan.State == "observed"
		for _, file := range out.Files {
			all = all && file.State == "matched"
		}
		if all {
			out.State = "installed_files_match_historical_plan"
		}
		return out
	}
	parent, err := inputs.open(p.parent, true, 0700, false)
	if err != nil || parent == nil {
		return finish()
	}
	lock, err := inputs.open(filepath.Join(p.parent, "owner.lock"), false, 0600, true)
	if err != nil || lock == nil || lock.stat.Size != 0 {
		return finish()
	}
	if unix.Flock(int(lock.file.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		out.Lock = prepareUnknown("owner_lock_busy")
		return finish()
	}
	out.Lock = prepareObserved()
	raw, err := inputs.read(filepath.Join(p.release, "host-plan.json"), 64*1024)
	if err != nil || raw == nil {
		return finish()
	}
	if bytesSHA(raw) != p.planSHA {
		out.Plan = prepareStatus{"mismatch", "plan_digest"}
		return finish()
	}
	plan, err := decodePreparePlan(raw)
	if err != nil {
		out.Plan = prepareUnknown("plan_schema")
		return finish()
	}
	for _, fact := range plan.Files {
		if *fact.Xattrs != installedEmptyXattrs {
			out.Plan = prepareUnknown("plan_xattrs_unsupported")
			return finish()
		}
	}
	out.Plan = prepareObserved()
	out.PlanExpires = plan.Expires
	for i, path := range prepareFixedPaths {
		if ctx.Err() != nil {
			break
		}
		var before func()
		if p.beforeBody != nil {
			before = func() { p.beforeBody(path) }
		}
		out.Files[i].prepareStatus = files.observe(ctx, filepath.Join(p.root, path), plan.Files[path], before)
	}
	return finish()
}

func writeInstalledReport(output io.Writer, report installedReport) error {
	raw, err := json.Marshal(report)
	if err != nil || len(raw) > 128*1024 {
		return errRefused
	}
	raw = append(raw, '\n')
	if n, err := output.Write(raw); err != nil || n != len(raw) {
		return errRefused
	}
	return nil
}

func runInstalled(ctx context.Context, args []string, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"installed_observation_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	f := flag.NewFlagSet("inspect-installed", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	release := f.String("release-sha256", "", "exact historical interpretation manifest")
	plan := f.String("plan-sha256", "", "exact historical host plan")
	if f.Parse(args) != nil || f.NArg() != 0 || *release != prepareReleaseSHA || *plan != preparePlanSHA {
		return fail()
	}
	stage = "identity"
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return fail()
	}
	stage = "environment"
	if !controlledEnvironment(os.Environ()) {
		return fail()
	}
	stage = "producer"
	producer, err := runningDigest()
	if err != nil {
		return fail()
	}
	p := installedReader{files: protectedFiles{uid: 0, boundary: "/"}, parent: "/var/lib/nucleus-halo-first-connection-inputs/" + prepareOperation, release: "/var/lib/nucleus-halo-rebind-code/" + prepareReleaseSHA, planSHA: preparePlanSHA, root: "/"}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	report := p.observe(ctx, producer)
	stage = "report"
	if writeInstalledReport(output, report) != nil {
		return fail()
	}
	return 0
}
