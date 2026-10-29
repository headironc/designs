package lifecycle

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type prepareStatus struct {
	State string `json:"state"`
	Code  string `json:"code,omitempty"`
}
type prepareRecordStatus struct {
	Name     string        `json:"name"`
	Presence prepareStatus `json:"presence"`
	Schema   prepareStatus `json:"schema"`
}
type prepareMemberStatus struct {
	Name  string `json:"name"`
	State string `json:"state"`
}
type prepareReleaseObservation struct {
	prepareStatus
	Manifest     prepareStatus         `json:"manifest"`
	Plan         prepareStatus         `json:"plan"`
	PlanExpires  *int64                `json:"historical_plan_expires_at"`
	Adapters     prepareStatus         `json:"adapters_directory"`
	Members      []prepareMemberStatus `json:"members"`
	OtherEntries int                   `json:"other_entries"`
	Content      string                `json:"member_content"`
}
type prepareHistorical struct {
	Repository           string                           `json:"repository"`
	RepositoryID         string                           `json:"old_garm_repository_id"`
	Controller           string                           `json:"controller_id"`
	Boot                 string                           `json:"boot_id"`
	ControllerInvocation string                           `json:"controller_invocation"`
	AppID                int64                            `json:"app_id"`
	Installation         int64                            `json:"source_installation_id"`
	TargetInstallation   int64                            `json:"target_installation_id"`
	Services             map[string]prepareUnitProjection `json:"services"`
}
type prepareReport struct {
	Version            int                       `json:"version"`
	Operation          string                    `json:"operation"`
	Amendment          string                    `json:"amendment"`
	ReleaseSHA         string                    `json:"interpretation_manifest_sha256"`
	PlanSHA            string                    `json:"interpretation_plan_sha256"`
	BootstrapSHA       string                    `json:"interpretation_bootstrap_sha256"`
	ObserverSHA        string                    `json:"observer_sha256"`
	ObservedAt         string                    `json:"observed_at"`
	State              string                    `json:"state"`
	Lock               prepareStatus             `json:"lock"`
	Release            prepareReleaseObservation `json:"release_staging"`
	AmendmentDirectory prepareStatus             `json:"amendment_directory"`
	ServicesDirectory  prepareStatus             `json:"services_directory"`
	Records            []prepareRecordStatus     `json:"records"`
	Publication        prepareStatus             `json:"publication"`
	CrossBinding       prepareStatus             `json:"cross_binding"`
	BeforeHash         prepareStatus             `json:"before_hash"`
	OtherEntries       int                       `json:"later_or_unknown_entries"`
	Pending            []prepareMemberStatus     `json:"pending"`
	Historical         *prepareHistorical        `json:"historical,omitempty"`
	Current            string                    `json:"current_installed_prestate"`
	Gaps               []string                  `json:"provenance_gaps"`
	OperationComplete  bool                      `json:"operation_complete"`
	Recovered          bool                      `json:"recovered"`
	Retry              bool                      `json:"retry_allowed"`
	Enable             bool                      `json:"enable_allowed"`
	ServiceWrite       bool                      `json:"service_write_allowed"`
	Cleanup            bool                      `json:"cleanup_allowed"`
	Migration          bool                      `json:"migration_allowed"`
	VMAcceptance       bool                      `json:"vm_acceptance"`
}
type prepareReader struct {
	files               protectedFiles
	parent, release     string
	releaseSHA, planSHA string
	beforeRecheck       func()
}

func prepareUnknown(code string) prepareStatus { return prepareStatus{"unknown", code} }
func prepareObserved() prepareStatus           { return prepareStatus{State: "observed"} }
func (p prepareReader) releaseObservation(s *prepareFiles) (prepareReleaseObservation, *preparePlan) {
	out := prepareReleaseObservation{prepareStatus: prepareUnknown("release_unavailable"), Manifest: prepareUnknown("manifest_unavailable"), Plan: prepareUnknown("plan_unavailable"), Adapters: prepareUnknown("adapters_unavailable"), Members: []prepareMemberStatus{}, Content: "not_observed_this_slice"}
	dir, e := s.open(p.release, true, 0700, false)
	if e != nil {
		return out, nil
	}
	if dir == nil {
		out.prepareStatus = prepareStatus{State: "not_observed"}
		return out, nil
	}
	names := prepareNames(dir)
	allowed := map[string]bool{"runtime-manifest.json": true, "host-plan.json": true, "adapters": true}
	for _, name := range prepareMembers {
		if !strings.Contains(name, "/") {
			allowed[name] = true
		}
	}
	for name := range names {
		if !allowed[name] {
			out.OtherEntries++
		}
	}
	var sizes map[string]int64
	raw, e := s.read(filepath.Join(p.release, "runtime-manifest.json"), 64*1024)
	if e == nil && raw != nil {
		if bytesSHA(raw) != p.releaseSHA {
			out.Manifest = prepareStatus{"mismatch", "manifest_digest"}
		} else if sizes, e = decodePrepareManifest(raw); e == nil {
			out.Manifest = prepareObserved()
		} else {
			out.Manifest = prepareUnknown("manifest_schema")
		}
	} else if e == nil {
		out.Manifest = prepareStatus{State: "not_observed"}
	}
	var plan *preparePlan
	raw, e = s.read(filepath.Join(p.release, "host-plan.json"), 64*1024)
	if e == nil && raw != nil {
		if bytesSHA(raw) != p.planSHA {
			out.Plan = prepareStatus{"mismatch", "plan_digest"}
		} else if plan, e = decodePreparePlan(raw); e == nil {
			out.Plan = prepareObserved()
			out.PlanExpires = plan.Expires
		} else {
			out.Plan = prepareUnknown("plan_schema")
		}
	} else if e == nil {
		out.Plan = prepareStatus{State: "not_observed"}
	}
	adapters, e := s.open(filepath.Join(p.release, "adapters"), true, 0700, false)
	if e != nil {
		adapters = nil
	} else if adapters == nil {
		out.Adapters = prepareStatus{State: "not_observed"}
	} else {
		out.Adapters = prepareObserved()
	}
	if adapters != nil {
		for _, name := range adapters.names {
			if name != "recover-controller-dependencies.py" && name != "verify-controller-start.py" {
				out.OtherEntries++
			}
		}
	}
	all := out.Manifest.State == "observed" && out.Plan.State == "observed" && adapters != nil && out.OtherEntries == 0
	for _, name := range prepareMembers {
		state := "unknown"
		if strings.Contains(name, "/") && out.Adapters.State == "not_observed" {
			state = "not_observed"
		}
		if !strings.Contains(name, "/") || adapters != nil {
			mode := uint32(0600)
			if name == "credential" {
				mode = 0500
			}
			obj, err := s.open(filepath.Join(p.release, name), false, mode, false)
			if err == nil {
				if obj == nil {
					state = "not_observed"
				} else if size, ok := sizes[name]; ok && obj.stat.Size == size {
					state = "metadata_observed"
				} else {
					state = "partial_or_unbound"
				}
			}
		}
		out.Members = append(out.Members, prepareMemberStatus{name, state})
		all = all && state == "metadata_observed"
	}
	if all {
		out.prepareStatus = prepareStatus{State: "metadata_observed"}
	} else {
		out.prepareStatus = prepareUnknown("release_partial_or_unbound")
	}
	return out, plan
}

func (p prepareReader) observe(ctx context.Context, producer string) prepareReport {
	report, _ := p.observeWithLease(ctx, producer, nil)
	return report
}

// A credential window consumes the same historical observation while retaining
// the original owner lock and pinned records. It supplies its own operation
// authorization; this observation still does not close or restart old work.
func (p prepareReader) observeWithLease(ctx context.Context, producer string, consume func(prepareReport, func() error) error) (prepareReport, error) {
	out := prepareReport{Version: 1, Operation: prepareOperation, Amendment: prepareAmendment, ReleaseSHA: p.releaseSHA, PlanSHA: p.planSHA, BootstrapSHA: prepareBootstrapSHA, ObserverSHA: producer, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), State: "unknown", Lock: prepareUnknown("owner_lock_unavailable"), AmendmentDirectory: prepareUnknown("amendment_unavailable"), ServicesDirectory: prepareUnknown("services_unavailable"), Publication: prepareUnknown("records_incomplete"), CrossBinding: prepareUnknown("binding_unavailable"), BeforeHash: prepareUnknown("before_hash_unavailable"), Current: "not_observed_this_slice", Pending: []prepareMemberStatus{}, Gaps: []string{"historical_producer_execution_unproven", "service_prestate_original_digest_missing", "external_preflight_not_reperformed", "credentials_not_decrypted_or_used", "current_installed_prestate_not_observed"}}
	paths := []string{"repair.intent.json", "before.json", "services/service-binding.json", "services/services-before.json", "prepare.complete.json"}
	for _, name := range paths {
		out.Records = append(out.Records, prepareRecordStatus{name, prepareUnknown("not_read"), prepareUnknown("not_read")})
	}
	if !digestPattern.MatchString(producer) {
		out.ObserverSHA = ""
		out.Publication = prepareUnknown("observer_producer_unavailable")
		return out, errRefused
	}
	r := newPrepareFiles(p.files)
	defer r.close()
	var plan *preparePlan
	out.Release, plan = p.releaseObservation(r)
	s := newPrepareFiles(p.files)
	defer s.close()
	finish := func() (prepareReport, error) {
		if p.beforeRecheck != nil {
			p.beforeRecheck()
		}
		if !r.unchanged(ctx) {
			out.Release.prepareStatus = prepareUnknown("release_staging_changed")
			out.State = "unknown"
			out.CrossBinding = prepareUnknown("release_staging_changed")
			out.Historical = nil
		}
		if !s.unchanged(ctx) {
			out.State = "unknown"
			out.Publication = prepareUnknown("records_changed")
			out.CrossBinding = prepareUnknown("records_changed")
			out.BeforeHash = prepareUnknown("records_changed")
			out.Historical = nil
		}
		if ctx.Err() != nil {
			out.State = "unknown"
			out.Publication = prepareUnknown("observation_cancelled")
		}
		if consume == nil {
			return out, nil
		}
		if out.Lock.State != "observed" {
			return out, errRefused
		}
		recheck := func() error {
			if !r.unchanged(ctx) || !s.unchanged(ctx) {
				return errRefused
			}
			return nil
		}
		if recheck() != nil {
			return out, errRefused
		}
		return out, consume(out, recheck)
	}
	parent, e := s.open(p.parent, true, 0700, false)
	if e != nil || parent == nil {
		return finish()
	}
	lock, e := s.open(filepath.Join(p.parent, "owner.lock"), false, 0600, true)
	if e != nil || lock == nil || lock.stat.Size != 0 {
		return finish()
	}
	if unix.Flock(int(lock.file.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		out.Lock = prepareUnknown("owner_lock_busy")
		return finish()
	}
	out.Lock = prepareObserved()
	amendment := filepath.Join(p.parent, prepareAmendment)
	dir, e := s.open(amendment, true, 0700, false)
	if e != nil {
		return finish()
	}
	if dir == nil {
		out.AmendmentDirectory = prepareStatus{State: "not_observed"}
		return finish()
	}
	out.AmendmentDirectory = prepareObserved()
	services, e := s.open(filepath.Join(amendment, "services"), true, 0700, false)
	if e != nil {
		out.ServicesDirectory = prepareUnknown("services_unsafe")
	} else if services == nil {
		out.ServicesDirectory = prepareStatus{State: "not_observed"}
	} else {
		out.ServicesDirectory = prepareObserved()
	}
	allowed := map[string]bool{"repair.intent.json": true, "before.json": true, "prepare.complete.json": true, "services": true}
	for _, name := range []string{"repair.intent.json", "before.json", "prepare.complete.json"} {
		allowed[name+".pending"] = true
	}
	pending := false
	for _, name := range dir.names {
		if strings.HasSuffix(name, ".pending") && allowed[name] {
			pending = true
		}
		if !allowed[name] {
			out.OtherEntries++
		}
	}
	if services != nil {
		for _, name := range services.names {
			if name == "service-binding.json.pending" || name == "services-before.json.pending" {
				pending = true
			} else if name != "service-binding.json" && name != "services-before.json" {
				out.OtherEntries++
			}
		}
	}
	for _, name := range paths {
		parentNames := prepareNames(dir)
		if strings.HasPrefix(name, "services/") {
			parentNames = prepareNames(services)
		}
		if !parentNames[filepath.Base(name)+".pending"] {
			continue
		}
		obj, err := s.open(filepath.Join(amendment, name+".pending"), false, 0600, false)
		state := "unsafe_pending"
		if err == nil && obj != nil {
			state = "pending_observed"
		}
		out.Pending = append(out.Pending, prepareMemberStatus{name + ".pending", state})
	}
	var before *prepareBefore
	var binding prepareBinding
	var prestate preparePrestate
	var complete prepareComplete
	var views map[string]prepareUnitProjection
	var beforeSum string
	valid := true
	gap := false
	prefix := true
	present := 0
	for i, name := range paths {
		if strings.HasPrefix(name, "services/") && services == nil {
			out.Records[i].Presence = prepareUnknown("services_unreadable")
			if out.ServicesDirectory.State == "not_observed" {
				out.Records[i].Presence = prepareStatus{State: "not_observed"}
				gap = true
			}
			valid = false
			continue
		}
		raw, err := s.read(filepath.Join(amendment, name), 16*1024*1024)
		if err != nil {
			out.Records[i].Presence = prepareUnknown("record_unsafe_or_unreadable")
			valid = false
			continue
		}
		if raw == nil {
			out.Records[i].Presence = prepareStatus{State: "not_observed"}
			valid = false
			gap = true
			continue
		}
		present++
		prefix = prefix && !gap
		out.Records[i].Presence = prepareObserved()
		ok := prepareReceiptBytes(raw)
		if ok {
			switch i {
			case 0:
				var intent prepareIntent
				ok = strictObject(raw, &intent) == nil && intent.Operation != nil && *intent.Operation == prepareOperation && intent.Amendment != nil && *intent.Amendment == prepareAmendment && intent.Approval != nil && *intent.Approval == prepareApproval
			case 1:
				before, err = decodePrepareBefore(raw)
				ok = err == nil
				if ok {
					beforeSum = bytesSHA(raw[:len(raw)-1])
				}
			case 2:
				ok = strictObject(raw, &binding) == nil && binding.Boot != nil && uuidPattern.MatchString(*binding.Boot) && validPrepareFacts(binding.Before) && validPrepareFacts(binding.After)
			case 3:
				ok = strictObject(raw, &prestate) == nil && prestate.Host != nil && validPrepareFacts(prestate.Host.Files)
				if ok {
					views, err = decodePrepareServices(prestate.Services)
					ok = err == nil
				}
			case 4:
				ok = strictObject(raw, &complete) == nil && complete.BeforeSHA != nil && digestPattern.MatchString(*complete.BeforeSHA)
			}
		}
		clear(raw)
		if ok {
			out.Records[i].Schema = prepareObserved()
		} else {
			out.Records[i].Schema = prepareUnknown("record_schema_or_binding")
			valid = false
		}
	}
	if before != nil {
		defer func() {
			clear(before.Payload)
			clear(before.Webhook)
			for _, row := range [][]json.RawMessage{before.RepositoryRow, before.CredentialRow} {
				for _, v := range row {
					clear(v)
				}
			}
		}()
	}
	for i, v := range out.Pending {
		pendingObject := s.objects[filepath.Join(amendment, v.Name)]
		target := s.objects[filepath.Join(amendment, strings.TrimSuffix(v.Name, ".pending"))]
		if pendingObject != nil && target != nil && pendingObject.file != nil && target.file != nil && pendingObject.stat.Mode == unix.S_IFREG|0600 && pendingObject.stat.Uid == p.files.uid && pendingObject.stat.Nlink == 2 && pendingObject.stat.Dev == target.stat.Dev && pendingObject.stat.Ino == target.stat.Ino {
			out.Pending[i].State = "linked_publication_incomplete"
		}
	}
	out.Publication = prepareUnknown("records_incomplete")
	if pending {
		out.Publication = prepareUnknown("publication_incomplete")
	} else if !prefix {
		out.Publication = prepareStatus{"inconsistent", "record_order"}
	} else if out.OtherEntries > 0 {
		out.Publication = prepareUnknown("later_or_unknown_entries")
	} else if present == 5 {
		out.Publication = prepareObserved()
	}
	if beforeSum != "" && complete.BeforeSHA != nil {
		if *complete.BeforeSHA == beforeSum {
			out.BeforeHash = prepareObserved()
		} else {
			out.BeforeHash = prepareStatus{"mismatch", "before_digest"}
		}
	}
	if valid && plan != nil {
		if !prepareEqualFacts(before.Files, binding.Before) || !prepareEqualFacts(before.Files, binding.After) || !prepareEqualFacts(before.Files, prestate.Host.Files) || !prepareEqualFacts(before.Files, plan.Files) || *binding.Boot != *plan.Boot {
			out.CrossBinding = prepareStatus{"mismatch", "historical_binding_conflict"}
		} else if *before.Invocation != views[controllerUnit].Invocation {
			out.CrossBinding = prepareStatus{"mismatch", "historical_invocation_conflict"}
		} else {
			out.CrossBinding = prepareObserved()
			out.Historical = &prepareHistorical{"silence-force/nucleus", prepareOldRepository, prepareController, *binding.Boot, *before.Invocation, *before.AppID, *before.Installation, *plan.Installation, views}
		}
	}
	if valid && out.Publication.State == "observed" && out.CrossBinding.State == "observed" && out.BeforeHash.State == "observed" {
		out.State = "prepare_records_consistent"
	}
	return finish()
}
