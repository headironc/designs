package lifecycle

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cloudbase/garm/params"
)

// The accepted source is a trusted input, not an ownership assertion inferred
// from finding a same-name object in a live inventory.
type connectionPlan struct {
	Version        int                         `json:"version"`
	Source         observerPlan                `json:"credential_source"`
	RepositoryMode string                      `json:"repository_mode"`
	Repository     string                      `json:"accepted_repository_id"`
	RepositorySHA  string                      `json:"accepted_repository_sha256"`
	WebhookSHA     string                      `json:"webhook_secret_sha256"`
	TemplateSHA    string                      `json:"template_sha256"`
	BootstrapSHA   string                      `json:"bootstrap_template_sha256"`
	ImageSHA       string                      `json:"image_fingerprint"`
	Scale          params.CreateScaleSetParams `json:"scaleset"`
}

func decodeConnectionPlan(raw []byte) (connectionPlan, error) {
	var p connectionPlan
	if len(raw) > 64*1024 || strictObject(raw, &p) != nil || p.Version != 1 {
		return connectionPlan{}, errRefused
	}
	source, err := json.Marshal(p.Source)
	if err != nil {
		return connectionPlan{}, errRefused
	}
	if _, err := decodeObserverPlan(source); err != nil {
		return connectionPlan{}, errRefused
	}
	if p.RepositoryMode != "create" && p.RepositoryMode != "existing" || p.RepositoryMode == "create" && (p.Repository != "" || p.RepositorySHA != "") || p.RepositoryMode == "existing" && (!uuidPattern.MatchString(p.Repository) || !digestPattern.MatchString(p.RepositorySHA)) {
		return connectionPlan{}, errRefused
	}
	for _, v := range []string{p.WebhookSHA, p.TemplateSHA, p.BootstrapSHA, p.ImageSHA} {
		if !digestPattern.MatchString(v) {
			return connectionPlan{}, errRefused
		}
	}
	s := p.Scale
	if s.Validate() != nil || s.ScaleSetID != 0 || s.Enabled || s.MinIdleRunners != 0 || s.MaxRunners != 1 || !s.DisableUpdate || s.EnableShell || s.ProviderName != "halo-lxd" || s.OSType != "linux" || s.OSArch != "amd64" || s.GitHubRunnerGroup != "Default" || s.TemplateID == nil || *s.TemplateID == 0 || uint64(*s.TemplateID) > 4294967295 || len(s.Labels) != 0 || len(s.Name) > 100 || !repoPart.MatchString(s.Name) || !repoPart.MatchString(s.Image) || s.Flavor != "halo-ci" || s.RunnerPrefix.Prefix == "" || s.RunnerBootstrapTimeout == 0 || s.RunnerBootstrapTimeout > uint(^uint32(0)) {
		return connectionPlan{}, errRefused
	}
	var extras struct {
		DisableUpdates *bool  `json:"disable_updates"`
		Template       []byte `json:"runner_install_template"`
	}
	if len(s.ExtraSpecs) > 8192 || strictObject(s.ExtraSpecs, &extras) != nil || extras.DisableUpdates == nil || !*extras.DisableUpdates || len(extras.Template) == 0 || bytesSHA(extras.Template) != p.BootstrapSHA {
		return connectionPlan{}, errRefused
	}
	return p, nil
}

var actionsBasePath = regexp.MustCompile(`^/(?:[A-Za-z0-9_-]+/)+$`)

type connectionActions struct {
	URL     string `json:"url"`
	Token   string `json:"token"`
	Expires int64  `json:"expires_at"`
}

// This is a bounded expiry projection from the protected HTTPS response. It
// does not assert that a local JWT signature or server revocation was checked.
func parseConnectionActions(raw []byte, received, until int64) (connectionActions, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	var wire struct {
		URL   *string `json:"url"`
		Token *string `json:"token"`
	}
	if unambiguous(raw) != nil || json.Unmarshal(raw, &wire) != nil || wire.URL == nil || wire.Token == nil || !validToken(*wire.Token) {
		return connectionActions{}, errRefused
	}
	u, err := url.Parse(*wire.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || !strings.HasSuffix(u.Hostname(), ".actions.githubusercontent.com") || u.Hostname() == ".actions.githubusercontent.com" || u.Host != u.Hostname() && u.Host != u.Hostname()+":443" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !actionsBasePath.MatchString(u.Path) {
		return connectionActions{}, errRefused
	}
	expires, err := connectionJWTExpiry(*wire.Token)
	if err != nil || expires <= received || expires < until {
		return connectionActions{}, errRefused
	}
	return connectionActions{*wire.URL, *wire.Token, expires}, nil
}

func connectionJWTExpiry(token string) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] == "" {
		return 0, errRefused
	}
	if signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2]); err != nil || len(signature) == 0 {
		return 0, errRefused
	}
	header, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return 0, errRefused
	}
	var h struct {
		Algorithm *string `json:"alg"`
	}
	if unambiguous(header) != nil || json.Unmarshal(header, &h) != nil || h.Algorithm == nil || *h.Algorithm == "" || strings.EqualFold(*h.Algorithm, "none") {
		return 0, errRefused
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || unambiguous(payload) != nil {
		return 0, errRefused
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil || claims == nil {
		return 0, errRefused
	}
	var expires int64
	if json.Unmarshal(claims["exp"], &expires) != nil || expires <= 0 || time.Unix(expires, 0).Year() > 9999 {
		return 0, errRefused
	}
	return expires, nil
}

type connectionBinding struct {
	Version            int    `json:"version"`
	Request            string `json:"request_sha256"`
	Producer           string `json:"producer_sha256"`
	Controller         string `json:"controller_id"`
	Installation       int64  `json:"installation_id"`
	Repository         string `json:"garm_repository_id"`
	ScaleSet           uint   `json:"garm_scaleset_id"`
	GitHubScaleSet     int    `json:"github_scaleset_id"`
	RepositoryResponse string `json:"repository_response_sha256"`
	ScaleResponse      string `json:"scaleset_response_sha256"`
	GARMObservation    string `json:"garm_observation_sha256"`
	GitHubObservation  string `json:"github_observation_sha256"`
	Enabled            bool   `json:"enabled"`
	CredentialState    string `json:"credential_state"`
}
