package lifecycle

import (
	"encoding/json"
	"reflect"

	"github.com/cloudbase/garm/params"
)

// Required wire facts are checked before native value fields erase absence.
// GARM's explicitly omitempty fields still retain their zero-value meaning.
func connectionWire(raw []byte, required ...string) bool {
	if unambiguous(raw) != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for _, name := range required {
		if len(fields[name]) == 0 || string(fields[name]) == "null" {
			return false
		}
	}
	for _, name := range []string{"enabled", "agent_mode", "min_idle_runners", "max_runners", "desired_runner_count", "instances", "disable_update", "enable_shell"} {
		if value, present := fields[name]; present && string(value) == "null" {
			return false
		}
	}
	return true
}

func connectionRepository(raw []byte, p connectionPlan, live bool) (params.Repository, error) {
	var repo params.Repository
	if !connectionWire(raw, "id", "owner", "name", "credentials_id", "agent_mode", "endpoint", "created_at") || json.Unmarshal(raw, &repo) != nil || !uuidPattern.MatchString(repo.ID) || repo.Owner != "headironc" || repo.Name != "nucleus" || repo.CredentialsID != uint(p.Source.Credential) || repo.AgentMode || !publicEndpoint(repo.Endpoint) || repo.Endpoint.Name != p.Source.Endpoint || repo.CreatedAt.IsZero() || repo.PoolBalancerType != "" || len(repo.Pools) != 0 || live && (!repo.PoolManagerStatus.IsRunning || repo.PoolManagerStatus.FailureReason != "") {
		return params.Repository{}, errRefused
	}
	return repo, nil
}

func connectionRepositoryIdentity(repo params.Repository) string {
	// Runtime manager status is checked separately and cannot change a prior
	// accepted repository's immutable identity projection.
	raw, _ := json.Marshal(struct {
		ID, Owner, Name string
		Credentials     uint
		Agent           bool
		Endpoint        params.ForgeEndpoint
		Created         string
		Balancer        params.PoolBalancerType
	}{repo.ID, repo.Owner, repo.Name, repo.CredentialsID, repo.AgentMode, repo.Endpoint, repo.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), repo.PoolBalancerType})
	return bytesSHA(raw)
}

func connectionScale(raw []byte, p connectionPlan, repository string, live bool) (params.ScaleSet, error) {
	var scale params.ScaleSet
	if !connectionWire(raw, "id", "scale_set_id", "repo_id", "name", "provider_name", "image", "flavor", "os_type", "os_arch", "runner_prefix", "runner_bootstrap_timeout", "template_id", "github-runner-group", "disable_update", "enable_shell", "created_at", "state", "extra_specs") || json.Unmarshal(raw, &scale) != nil {
		return scale, errRefused
	}
	s := p.Scale
	if scale.ID == 0 || uint64(scale.ID) > 4294967295 || scale.ScaleSetID <= 0 || scale.RepoID != repository || scale.Name != s.Name || scale.Enabled || scale.MinIdleRunners != 0 || scale.MaxRunners != 1 || !scale.DisableUpdate || scale.EnableShell || scale.ProviderName != s.ProviderName || scale.Image != s.Image || scale.Flavor != s.Flavor || scale.OSType != s.OSType || scale.OSArch != s.OSArch || scale.Prefix != s.Prefix || scale.RunnerBootstrapTimeout != s.RunnerBootstrapTimeout || scale.TemplateID != *s.TemplateID || scale.GitHubRunnerGroup != "Default" || len(scale.Instances) != 0 || scale.DesiredRunnerCount != 0 || scale.OrgID != "" || scale.EnterpriseID != "" || len(scale.Tags) != 0 || scale.CreatedAt.IsZero() || scale.State != params.ScaleSetCreated && (live || scale.State != params.ScaleSetPendingCreate) {
		return scale, errRefused
	}
	var actual, expected struct {
		DisableUpdates *bool  `json:"disable_updates"`
		Template       []byte `json:"runner_install_template"`
	}
	if strictObject(scale.ExtraSpecs, &actual) != nil || strictObject(s.ExtraSpecs, &expected) != nil || !reflect.DeepEqual(actual, expected) {
		return scale, errRefused
	}
	return scale, nil
}

func connectionRemote(raw []byte, p connectionPlan, scale params.ScaleSet) error {
	var response params.RunnerScaleSetsResponse
	if !connectionWire(raw, "count", "value") || json.Unmarshal(raw, &response) != nil || response.Count != 1 || len(response.RunnerScaleSets) != 1 {
		return errRefused
	}
	r := response.RunnerScaleSets[0]
	if r.ID != scale.ScaleSetID || r.Name != p.Scale.Name || r.RunnerGroupID != 1 || r.Enabled == nil || *r.Enabled || !r.RunnerSetting.Ephemeral || !r.RunnerSetting.DisableUpdate || r.CreatedOn.IsZero() || len(r.Labels) != 1 || r.Labels[0].Name != p.Scale.Name || r.Labels[0].Type != "System" {
		return errRefused
	}
	return nil
}
