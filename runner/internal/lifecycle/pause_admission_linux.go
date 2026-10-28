package lifecycle

import (
	"context"
	"encoding/json"

	"github.com/cloudbase/garm/client/pools"
	"github.com/cloudbase/garm/client/scalesets"
	httptransport "github.com/go-openapi/runtime/client"
)

// This is controller-global disabled configuration evidence, not a claim that
// prior manager operations, credentials or resources have closed.
func (p *platforms) pauseAdmission(ctx context.Context, r receipt) (string, error) {
	job := p.diagnoseJob(ctx, r)
	if job.State != "terminal" {
		return "", errRefused
	}
	current := p.diagnoseGARM(ctx, r)
	if (current.State != "present" && current.State != "absent_verified") || current.Admission != "disabled_observed" {
		return "", errRefused
	}
	auth := httptransport.BearerToken(p.token)
	sets, err := p.garm.Scalesets.ListScalesets(scalesets.NewListScalesetsParamsWithContext(ctx), auth)
	if err != nil || sets == nil || sets.Payload == nil || len(sets.Payload) > 10000 {
		return "", errRefused
	}
	ordinary, err := p.garm.Pools.ListPools(pools.NewListPoolsParamsWithContext(ctx), auth)
	if err != nil || ordinary == nil || ordinary.Payload == nil || len(ordinary.Payload) > 10000 {
		return "", errRefused
	}
	first, _ := json.Marshal([]string{"job", job.Conclusion, current.State, current.TargetStatus})
	facts := []string{string(first)}
	seen := map[uint]bool{}
	found := false
	for _, s := range sets.Payload {
		scope, err := admissionScope(s.RepoID, s.OrgID, s.EnterpriseID)
		if err != nil || s.ID == 0 || uint64(s.ID) > 4294967295 || seen[s.ID] || s.Name == "" || s.ProviderName == "" || s.Enabled || !publicEndpoint(s.Endpoint) || s.ScaleSetID < 0 {
			return "", errRefused
		}
		seen[s.ID] = true
		if s.RepoID == r.Expected.GARMRepository && s.Name == r.Observed.Guard.ScaleSet && uint64(s.ID) != uint64(r.Expected.ScaleSet) {
			return "", errRefused
		}
		if uint64(s.ID) == uint64(r.Expected.ScaleSet) {
			b := r.Observed.Guard
			if found || s.ScaleSetID != r.Observed.GitHubScaleSetID || s.RepoID != r.Expected.GARMRepository || s.OrgID != "" || s.EnterpriseID != "" || s.Name != b.ScaleSet || s.ProviderName != "halo-lxd" || !publicEndpoint(s.Endpoint) {
				return "", errRefused
			}
			found = true
		}
		raw, _ := json.Marshal([]any{"scale", s.ID, s.ScaleSetID, scope, s.Name, s.ProviderName, s.Endpoint.EndpointType, s.Endpoint.BaseURL, s.Endpoint.APIBaseURL})
		facts = append(facts, string(raw))
	}
	if !found {
		return "", errRefused
	}
	poolIDs := map[string]bool{}
	for _, v := range ordinary.Payload {
		scope, err := admissionScope(v.RepoID, v.OrgID, v.EnterpriseID)
		if err != nil || !uuidPattern.MatchString(v.ID) || poolIDs[v.ID] || v.ProviderName == "" || v.Enabled || !publicEndpoint(v.Endpoint) {
			return "", errRefused
		}
		poolIDs[v.ID] = true
		raw, _ := json.Marshal([]string{"pool", v.ID, scope, v.ProviderName, string(v.Endpoint.EndpointType), v.Endpoint.BaseURL, v.Endpoint.APIBaseURL})
		facts = append(facts, string(raw))
	}
	// Re-read the controller/target/instance binding after both global lists.
	again := p.diagnoseGARM(ctx, r)
	if again.State != current.State || again.TargetStatus != current.TargetStatus || again.Admission != current.Admission {
		return "", errRefused
	}
	return inventoryIdentity(facts), nil
}

func admissionScope(repo, org, enterprise string) (string, error) {
	result := ""
	for _, pair := range []struct{ kind, id string }{{"repository", repo}, {"organization", org}, {"enterprise", enterprise}} {
		if pair.id == "" {
			continue
		}
		if result != "" || !uuidPattern.MatchString(pair.id) {
			return "", errRefused
		}
		result = pair.kind + "/" + pair.id
	}
	if result == "" {
		return "", errRefused
	}
	return result, nil
}

// Global inventory Enabled uses omitempty for false. Inspect its wire before
// the SDK's bool and string zero values erase null and required-field absence.
func admissionWire(path string, raw []byte) bool {
	if path != "/api/v1/scalesets" && path != "/api/v1/pools" {
		return true
	}
	var rows []map[string]json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) > 10000 {
		return false
	}
	for _, row := range rows {
		canonical := map[string]json.RawMessage{}
		for k, v := range row {
			canonical[foldJSONName(k)] = v
		}
		required := []string{"id", "provider_name", "endpoint"}
		if path == "/api/v1/scalesets" {
			required = append(required, "name")
		}
		for _, key := range required {
			v := canonical[foldJSONName(key)]
			if len(v) == 0 || string(v) == "null" {
				return false
			}
		}
		if v, ok := canonical[foldJSONName("enabled")]; ok && string(v) != "true" && string(v) != "false" {
			return false
		}
		for _, key := range []string{"repo_id", "org_id", "enterprise_id", "scale_set_id"} {
			if v, ok := canonical[foldJSONName(key)]; ok && string(v) == "null" {
				return false
			}
		}
		var ep *endpointPresence
		if json.Unmarshal(canonical[foldJSONName("endpoint")], &ep) != nil || !ep.complete() {
			return false
		}
	}
	return true
}
