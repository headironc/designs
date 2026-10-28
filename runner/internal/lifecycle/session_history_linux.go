package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const sessionLegacyOperationSHA = "1fedd744204deb9881c875b029de44b8661b9ab89adc7b3f1aa0da5c59bc050f"
const sessionLegacyScriptSHA = "805cd2bdafcf15e0a9d87bdba4006aba25f59f539419eb8af564c280d6dad679"

type sessionHistory struct {
	prepare   prepareReader
	legacyDir string
	legacySHA string
}

// The legacy operation predates the root-owned /var/lib store. Its directory
// has user-owned ancestors, so authenticate the fixed directory itself, the
// existing original lock, and the frozen content digest. Never create a lock
// or widen protectedFiles' parent contract for other inputs.
func (h sessionHistory) withServiceHistory(ctx context.Context, producer, controller string, consume func(prepareHistorical, string, func() error) error) error {
	if consume == nil || !uuidPattern.MatchString(controller) || !digestPattern.MatchString(h.legacySHA) {
		return errRefused
	}
	_, err := h.prepare.observeWithLease(ctx, producer, func(report prepareReport, prepareCheck func() error) error {
		if report.State != "prepare_records_consistent" || report.Historical == nil || report.Historical.Controller != controller || report.OtherEntries != 0 || len(report.Pending) != 0 {
			return errRefused
		}
		dir, err := openPath(h.legacyDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOATIME, 0)
		if err != nil {
			return errRefused
		}
		defer dir.Close()
		var before unix.Stat_t
		if unix.Fstat(int(dir.Fd()), &before) != nil || before.Uid != h.prepare.files.uid || before.Mode != unix.S_IFDIR|0700 || !samePath(dir, h.legacyDir) {
			return errRefused
		}
		s := newPrepareFiles(protectedFiles{uid: h.prepare.files.uid, boundary: h.legacyDir})
		defer s.close()
		lock, err := s.open(filepath.Join(h.legacyDir, "owner.lock"), false, 0600, true)
		if err != nil || lock == nil || lock.stat.Size != 0 || unix.Flock(int(lock.file.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
			return errRefused
		}
		raw, err := s.read(filepath.Join(h.legacyDir, "operation.json"), 1024*1024)
		defer clear(raw)
		if err != nil || bytesSHA(raw) != h.legacySHA || sessionLegacyJobsSettled(raw) != nil {
			return errRefused
		}
		var original struct {
			SHA string `json:"original_sha256"`
		}
		if json.Unmarshal(raw, &original) != nil {
			return errRefused
		}
		state, err := s.read(filepath.Join(h.legacyDir, "original-state.json"), 1024*1024)
		defer clear(state)
		if err != nil || state == nil || bytesSHA(state) != original.SHA {
			return errRefused
		}
		check := func() error {
			var now unix.Stat_t
			if prepareCheck() != nil || !s.unchanged(ctx) || unix.Fstat(int(dir.Fd()), &now) != nil || !samePrepareStat(before, now) || !samePath(dir, h.legacyDir) {
				return errRefused
			}
			return nil
		}
		if check() != nil {
			return errRefused
		}
		return consume(*report.Historical, h.legacySHA, check)
	})
	return err
}

func (h sessionHistory) withAdmissionHistory(ctx context.Context, producer string, p sessionPlan, clock func() time.Time, consume func(string, func() error) error) error {
	if consume == nil || clock == nil {
		return errRefused
	}
	now := clock().Unix()
	if now <= 0 || now >= p.Expires {
		return errRefused
	}
	return h.withServiceHistory(ctx, producer, p.Controller, func(historical prepareHistorical, legacySHA string, serviceCheck func() error) error {
		s := newPrepareFiles(h.prepare.files)
		defer s.close()
		acquisitionSHA, err := h.acquisition(s, historical, now)
		if err != nil {
			return errRefused
		}
		bindings := map[string]string{"legacy_operation": legacySHA, "acquisition": acquisitionSHA}
		for _, name := range []string{"repair.intent.json", "before.json", "services/service-binding.json", "services/services-before.json", "prepare.complete.json"} {
			raw, err := s.read(filepath.Join(h.prepare.parent, prepareAmendment, name), 16*1024*1024)
			if err != nil || raw == nil {
				clear(raw)
				return errRefused
			}
			bindings[name] = bytesSHA(raw)
			clear(raw)
		}
		raw, err := json.Marshal(bindings)
		if err != nil {
			return errRefused
		}
		check := func() error {
			current := clock().Unix()
			if current < now || current >= p.Expires || serviceCheck() != nil || !s.unchanged(ctx) {
				return errRefused
			}
			return nil
		}
		if check() != nil {
			return errRefused
		}
		return consume(bytesSHA(raw), check)
	})
}

type sessionLegacyJob struct {
	Method   *string         `json:"method"`
	Mode     *string         `json:"mode"`
	Status   *string         `json:"status"`
	Started  *uint64         `json:"started_monotonic_us"`
	Path     *string         `json:"path,omitempty"`
	Result   *string         `json:"result,omitempty"`
	Error    *string         `json:"error_name,omitempty"`
	Readback json.RawMessage `json:"readback,omitempty"`
}

type sessionLegacyEvent struct {
	ID     *uint32 `json:"id"`
	Unit   *string `json:"unit"`
	Result *string `json:"result"`
}

// This projection only checks the original service requests. In particular,
// phase=restored does not establish the outcome of the old Login request.
func sessionLegacyJobsSettled(raw []byte) error {
	var record struct {
		Script   *string                    `json:"script_sha256"`
		Helper   *string                    `json:"helper_sha256"`
		Original *string                    `json:"original_sha256"`
		Phase    *string                    `json:"phase"`
		Reset    json.RawMessage            `json:"reset_failed"`
		Jobs     map[string]json.RawMessage `json:"jobs"`
		Events   map[string]json.RawMessage `json:"job_events"`
	}
	if unambiguous(raw) != nil || json.Unmarshal(raw, &record) != nil || record.Script == nil || *record.Script != sessionLegacyScriptSHA || record.Helper == nil || !digestPattern.MatchString(*record.Helper) || record.Original == nil || !digestPattern.MatchString(*record.Original) || record.Phase == nil || len(record.Jobs) == 0 || record.Events == nil {
		return errRefused
	}
	switch *record.Phase {
	case "prepared", "inject-intent", "stopped", "refusal-observed", "restore-intent", "state-restored", "restart-intent", "restored":
	default:
		return errRefused
	}
	if record.Reset != nil {
		var reset string
		if json.Unmarshal(record.Reset, &reset) != nil || reset != "done" {
			return errRefused
		}
	}
	events := map[string]sessionLegacyEvent{}
	for path, raw := range record.Events {
		var event sessionLegacyEvent
		if strictObject(raw, &event) != nil || event.ID == nil || *event.ID == 0 || path != fmt.Sprintf("/org/freedesktop/systemd1/job/%d", *event.ID) || event.Unit == nil || *event.Unit != controllerUnit || event.Result == nil || !pauseJobResult(*event.Result) {
			return errRefused
		}
		events[path] = event
	}
	methods := map[string]string{"initial-stop": "StopUnit", "test-start": "StartUnit", "quiesce": "StopUnit", "restore-start": "StartUnit"}
	paths := map[string]bool{}
	for label, raw := range record.Jobs {
		var job sessionLegacyJob
		if strictObject(raw, &job) != nil || job.Method == nil || methods[label] == "" || *job.Method != methods[label] || job.Mode == nil || (*job.Mode != "fail" && !(label == "quiesce" && *job.Mode == "replace")) || job.Started == nil || *job.Started == 0 || job.Status == nil {
			return errRefused
		}
		if job.Readback != nil {
			// A later original event may settle a job after an earlier
			// readback-only observation. Keep that field without using it
			// as a substitute for the event below.
			var view map[string]json.RawMessage
			if json.Unmarshal(job.Readback, &view) != nil || view == nil {
				return errRefused
			}
		}
		switch *job.Status {
		case "rejected":
			if job.Path != nil || job.Result != nil || job.Readback != nil || job.Error == nil || !sessionLegacyRejection(*job.Error) {
				return errRefused
			}
		case "terminal":
			if job.Path == nil || job.Result == nil || job.Error != nil || paths[*job.Path] {
				return errRefused
			}
			event, ok := events[*job.Path]
			if !ok || *event.Result != *job.Result {
				return errRefused
			}
			paths[*job.Path] = true
		default:
			// The old recovery's observed-running/stopped fallback has no
			// original terminal event and cannot authorize a new window.
			return errRefused
		}
	}
	return nil
}

func sessionLegacyRejection(name string) bool {
	switch name {
	case "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.systemd1.NoSuchUnit", "org.freedesktop.systemd1.TransactionIsDestructive", "org.freedesktop.systemd1.JobTypeNotApplicable", "org.freedesktop.systemd1.StartLimitHit":
		return true
	}
	return false
}
