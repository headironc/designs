package lifecycle

import (
	"context"
	"strconv"

	"github.com/cloudbase/garm/client/controller_info"
	"github.com/cloudbase/garm/client/instances"
	"github.com/cloudbase/garm/client/repositories"
	"github.com/cloudbase/garm/client/scalesets"
	"github.com/cloudbase/garm/params"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/google/go-github/v84/github"
)

func (p *platforms) diagnoseJob(ctx context.Context, original receipt) jobObservation {
	r := original.Expected
	b := original.Observed.Guard
	failure := jobObservation{State: "unknown", Code: "github_job_unavailable"}
	repo, _, err := p.github.Repositories.Get(ctx, r.Owner, r.Repository)
	if err != nil {
		return failure
	}
	if !validRepository(repo, r) {
		return jobObservation{State: "mismatch", Code: "repository_identity"}
	}
	run, _, err := p.github.Actions.GetWorkflowRunAttempt(ctx, r.Owner, r.Repository, r.RunID, r.Attempt, nil)
	if err != nil {
		return failure
	}
	if run == nil || run.ID == nil || run.RunAttempt == nil || run.HeadSHA == nil || run.Status == nil || !validRepository(run.Repository, r) {
		return failure
	}
	if *run.ID != r.RunID || *run.RunAttempt != r.Attempt || *run.HeadSHA != r.Commit {
		return jobObservation{State: "mismatch", Code: "run_identity"}
	}
	job, _, err := p.github.Actions.GetWorkflowJobByID(ctx, r.Owner, r.Repository, r.JobID)
	if err != nil {
		return failure
	}
	if job == nil || job.ID == nil || job.RunID == nil || job.RunAttempt == nil || job.HeadSHA == nil || job.Status == nil || job.RunnerID == nil || job.RunnerName == nil {
		return failure
	}
	if *job.ID != r.JobID || *job.RunID != r.RunID || *job.RunAttempt != int64(r.Attempt) || *job.HeadSHA != r.Commit || *job.RunnerID != b.AgentID || *job.RunnerName != b.Name {
		return jobObservation{State: "mismatch", Code: "job_identity"}
	}
	switch *job.Status {
	case "completed":
		if job.Conclusion == nil {
			return failure
		}
		switch *job.Conclusion {
		case "success", "failure", "cancelled", "timed_out", "neutral", "skipped", "action_required", "stale", "startup_failure":
			return jobObservation{State: "terminal", Conclusion: *job.Conclusion, RunState: *run.Status}
		}
	case "queued", "in_progress", "waiting", "pending", "requested":
		return jobObservation{State: "running", RunState: *run.Status}
	}
	return failure
}

func (p *platforms) diagnoseRegistration(ctx context.Context, r receipt) resourceObservation {
	seen := map[int64]bool{}
	names := map[string]bool{}
	total := -1
	matches := 0
	for page := 1; page <= 100; page++ {
		all, response, err := p.github.Actions.ListRunners(ctx, r.Expected.Owner, r.Expected.Repository, &github.ListRunnersOptions{ListOptions: github.ListOptions{Page: page, PerPage: 100}})
		if err != nil || all == nil || all.Runners == nil || response == nil || all.TotalCount < 0 || all.TotalCount > 10000 {
			return unknown("runner_inventory_unavailable")
		}
		if total != -1 && total != all.TotalCount {
			return unknown("runner_inventory_changed")
		}
		total = all.TotalCount
		for _, runner := range all.Runners {
			if runner == nil || runner.ID == nil || *runner.ID <= 0 || runner.Name == nil || *runner.Name == "" || seen[*runner.ID] || names[*runner.Name] {
				return unknown("runner_inventory_invalid")
			}
			seen[*runner.ID] = true
			names[*runner.Name] = true
			if *runner.ID == r.Observed.RunnerID || *runner.Name == r.Observed.RunnerName {
				if *runner.ID != r.Observed.RunnerID || *runner.Name != r.Observed.RunnerName {
					return resource("mismatch", 1)
				}
				matches++
			}
		}
		if response.NextPage == 0 {
			if len(seen) != total {
				return unknown("runner_inventory_incomplete")
			}
			if matches == 0 {
				return resource("absent_verified", 0)
			}
			return resource("present", matches)
		}
		if response.NextPage != page+1 || len(all.Runners) == 0 || len(seen) >= total {
			return unknown("runner_pagination_invalid")
		}
	}
	return unknown("runner_inventory_bound")
}

func (p *platforms) diagnoseGARM(ctx context.Context, r receipt) garmObservation {
	bad := func(code string) garmObservation { return garmObservation{resourceObservation: unknown(code)} }
	auth := httptransport.BearerToken(p.token)
	expected := r.Expected
	b := r.Observed.Guard
	c, err := p.garm.ControllerInfo.ControllerInfo(controller_info.NewControllerInfoParamsWithContext(ctx), auth)
	if err != nil || c == nil {
		return bad("garm_controller_unavailable")
	}
	if c.Payload.ControllerID.String() != expected.Controller {
		return garmObservation{resourceObservation: resource("mismatch", 0)}
	}
	repo, err := p.garm.Repositories.GetRepo(repositories.NewGetRepoParamsWithContext(ctx).WithRepoID(expected.GARMRepository), auth)
	if err != nil || repo == nil {
		return bad("garm_repository_unavailable")
	}
	if repo.Payload.ID != expected.GARMRepository || repo.Payload.Owner != expected.Owner || repo.Payload.Name != expected.Repository || !publicEndpoint(repo.Payload.Endpoint) {
		return garmObservation{resourceObservation: resource("mismatch", 0)}
	}
	id := strconv.FormatUint(uint64(expected.ScaleSet), 10)
	scale, err := p.garm.Scalesets.GetScaleSet(scalesets.NewGetScaleSetParamsWithContext(ctx).WithScalesetID(id), auth)
	if err != nil || scale == nil {
		return bad("garm_scale_unavailable")
	}
	if uint64(scale.Payload.ID) != uint64(expected.ScaleSet) || scale.Payload.ScaleSetID != r.Observed.GitHubScaleSetID || scale.Payload.RepoID != expected.GARMRepository || scale.Payload.Name != b.ScaleSet || scale.Payload.ProviderName != "halo-lxd" || scale.Payload.OrgID != "" || scale.Payload.EnterpriseID != "" || !publicEndpoint(scale.Payload.Endpoint) {
		return garmObservation{resourceObservation: resource("mismatch", 0)}
	}
	all, err := p.garm.Instances.ListInstances(instances.NewListInstancesParamsWithContext(ctx), auth)
	if err != nil || all == nil || all.Payload == nil || len(all.Payload) > 10000 {
		return bad("garm_inventory_unavailable")
	}
	target, err := p.garm.Instances.ListScaleSetInstances(instances.NewListScaleSetInstancesParamsWithContext(ctx).WithScalesetID(id), auth)
	if err != nil || target == nil || target.Payload == nil || len(target.Payload) > 10000 {
		return bad("garm_scale_inventory_unavailable")
	}
	seen := map[string]params.Instance{}
	names := map[string]bool{}
	matched := false
	status := ""
	for _, item := range all.Payload {
		if !uuidPattern.MatchString(item.ID) || item.Name == "" || names[item.Name] {
			return bad("garm_inventory_invalid")
		}
		if _, ok := seen[item.ID]; ok {
			return bad("garm_inventory_invalid")
		}
		seen[item.ID] = item
		names[item.Name] = true
		if item.ID == expected.Instance || item.Name == b.Name {
			if item.ID != expected.Instance || item.Name != b.Name || item.ProviderName != "halo-lxd" || item.ProviderID != "" && item.ProviderID != b.Name || item.AgentID != 0 && item.AgentID != b.AgentID || item.ScaleSetID != 0 && uint64(item.ScaleSetID) != uint64(expected.ScaleSet) || item.PoolID != "" {
				return garmObservation{resourceObservation: resource("mismatch", len(all.Payload))}
			}
			matched = true
			switch string(item.Status) {
			case "running", "stopped", "error", "pending_delete", "pending_force_delete", "deleting", "deleted", "pending_create", "creating":
				status = string(item.Status)
			default:
				return bad("garm_instance_status_invalid")
			}
			if item.ProviderID == "" || item.AgentID == 0 || item.ScaleSetID == 0 {
				return garmObservation{resourceObservation: unknown("garm_instance_binding_unassigned"), TargetStatus: status}
			}
		}
	}
	scaleSeen := map[string]bool{}
	for _, item := range target.Payload {
		original, ok := seen[item.ID]
		if !ok || scaleSeen[item.ID] || item.Name != original.Name || item.ProviderID != original.ProviderID || item.ProviderName != original.ProviderName || item.AgentID != original.AgentID || item.ScaleSetID != original.ScaleSetID || item.Status != original.Status || item.RunnerStatus != original.RunnerStatus || uint64(item.ScaleSetID) != uint64(expected.ScaleSet) {
			return bad("garm_inventory_inconsistent")
		}
		scaleSeen[item.ID] = true
	}
	for _, item := range all.Payload {
		if uint64(item.ScaleSetID) == uint64(expected.ScaleSet) && !scaleSeen[item.ID] {
			return bad("garm_inventory_inconsistent")
		}
	}
	state := "absent_verified"
	if len(all.Payload) > 0 {
		state = "residual"
	}
	if matched && len(all.Payload) == 1 {
		state = "present"
	}
	admission := "disabled_observed"
	if scale.Payload.Enabled {
		admission = "enabled_observed"
	}
	return garmObservation{resourceObservation: resource(state, len(all.Payload)), TargetStatus: status, Admission: admission}
}
