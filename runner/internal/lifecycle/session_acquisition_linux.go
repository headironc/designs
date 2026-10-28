package lifecycle

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"path/filepath"
	"strconv"
	"strings"
)

type sessionAcquisitionSource struct {
	Boot       *string                    `json:"boot_id"`
	Invocation *string                    `json:"controller_invocation"`
	Service    map[string]json.RawMessage `json:"service"`
	Config     *string                    `json:"garm_config_sha256"`
	Policy     *string                    `json:"policy_sha256"`
	Binary     *string                    `json:"garm_binary_sha256"`
	Credential *int64                     `json:"credential_id"`
	Name       *string                    `json:"credential_name"`
	Endpoint   *string                    `json:"endpoint_name"`
	Updated    *string                    `json:"credential_updated_at"`
	Payload    *string                    `json:"payload_sha256"`
	Repository *string                    `json:"source_repository_id"`
	Owner      *string                    `json:"source_owner"`
}

// Only the stopped, pre-mint acquisition profile can enter this window. A
// later acquisition or rebind request needs its own original recovery; it is
// never converted to a new GARM Login by interpreting a cleanup Boolean.
func (h sessionHistory) acquisition(s *prepareFiles, historical prepareHistorical, now int64) (string, error) {
	dir, err := s.open(h.prepare.parent, true, 0700, false)
	if err != nil || dir == nil || now <= 0 {
		return "", errRefused
	}
	allowed := map[string]bool{"owner.lock": true, prepareAmendment: true, "acquisition.intent.json": true, "acquisition.stopped.json": true, "source.json": true, "app-sign.intent.json": true, "app-sign.result.json": true, "app.read.json": true, "installation-before.read.json": true}
	for _, name := range dir.names {
		if !allowed[name] {
			return "", errRefused
		}
	}
	sums := map[string]string{}
	read := func(name string, out any) ([]byte, error) {
		raw, err := s.read(filepath.Join(h.prepare.parent, name), 1024*1024)
		if err != nil || raw == nil || !prepareReceiptBytes(raw) || strictObject(raw, out) != nil {
			clear(raw)
			return nil, errRefused
		}
		sums[name] = bytesSHA(raw)
		return raw, nil
	}
	var intent struct {
		Operation  *string `json:"operation"`
		Repository *int64  `json:"repository_id"`
		FullName   *string `json:"full_name"`
		OldLogin   *string `json:"old_login_status"`
		OldSHA     *string `json:"old_operation_sha256"`
		Approval   *string `json:"approval_message"`
	}
	raw, err := read("acquisition.intent.json", &intent)
	clear(raw)
	if err != nil || intent.Operation == nil || *intent.Operation != prepareOperation || intent.Repository == nil || *intent.Repository != 1432619931 || intent.FullName == nil || *intent.FullName != "headironc/nucleus" || intent.OldLogin == nil || *intent.OldLogin != "unknown" || intent.OldSHA == nil || *intent.OldSHA != h.legacySHA || intent.Approval == nil || *intent.Approval != "01a17d25-f7e0-7782-bf50-472c1dce74c0" {
		return "", errRefused
	}
	var stopped struct {
		Complete *bool `json:"complete"`
		Cleanup  *struct {
			Complete *bool `json:"complete"`
			Tokens   map[string]struct {
				State  *string `json:"state"`
				Reason *string `json:"reason"`
			} `json:"installation_tokens"`
			Revoked *bool `json:"child_sessions_revoked"`
		} `json:"cleanup"`
	}
	raw, err = read("acquisition.stopped.json", &stopped)
	clear(raw)
	if err != nil || stopped.Complete == nil || *stopped.Complete || stopped.Cleanup == nil || stopped.Cleanup.Complete == nil || !*stopped.Cleanup.Complete || stopped.Cleanup.Revoked == nil || *stopped.Cleanup.Revoked || len(stopped.Cleanup.Tokens) != 2 {
		return "", errRefused
	}
	for _, name := range []string{"audit-mint", "consumer-mint"} {
		v, ok := stopped.Cleanup.Tokens[name]
		if !ok || v.State == nil || *v.State != "not_attempted" || v.Reason == nil || *v.Reason != "no_mint_receipt" {
			return "", errRefused
		}
	}
	var source sessionAcquisitionSource
	raw, err = read("source.json", &source)
	defer clear(raw)
	if err != nil || !validSessionAcquisitionSource(source, historical) {
		return "", errRefused
	}
	var signIntent struct {
		Target  *string      `json:"target"`
		Payload *string      `json:"payload_sha256"`
		Started *json.Number `json:"started_at"`
	}
	signing, err := read("app-sign.intent.json", &signIntent)
	clear(signing)
	if err != nil || signIntent.Target == nil || *signIntent.Target != "local:Nucleiron" || signIntent.Payload == nil || *signIntent.Payload != bytesSHA(raw[:len(raw)-1]) || signIntent.Started == nil || !validSessionEpoch(*signIntent.Started, now) {
		return "", errRefused
	}
	var signResult struct {
		Phase    *string `json:"phase"`
		Response *struct {
			App     *int64  `json:"app_id"`
			Install *int64  `json:"installation_id"`
			Expires *int64  `json:"expires_at"`
			Token   *string `json:"token"`
		} `json:"response"`
	}
	signing, err = read("app-sign.result.json", &signResult)
	defer clear(signing)
	if err != nil || signResult.Phase == nil || *signResult.Phase != "response_received" || signResult.Response == nil {
		return "", errRefused
	}
	v := signResult.Response
	if v.App == nil || *v.App != historical.AppID || v.Install == nil || *v.Install != historical.Installation || v.Expires == nil || *v.Expires <= 0 || *v.Expires > now || v.Token == nil || !expiredSessionAppToken(*v.Token, *v.Expires, historical.AppID) {
		return "", errRefused
	}
	for _, name := range []string{"app.read.json", "installation-before.read.json"} {
		if !prepareNames(dir)[name] {
			continue
		}
		// These are GET observations, not write receipts. Their bodies do
		// not grant admission; retain only the original content binding.
		var observation map[string]json.RawMessage
		data, err := read(name, &observation)
		clear(data)
		if err != nil || observation == nil {
			return "", errRefused
		}
	}
	data, _ := json.Marshal(sums)
	return bytesSHA(data), nil
}

func validSessionEpoch(n json.Number, now int64) bool {
	if len(n) > 128 {
		return false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil || f <= 0 || f > float64(now) {
		return false
	}
	r, ok := new(big.Rat).SetString(string(n))
	return ok && r.Sign() > 0 && r.Cmp(big.NewRat(now, 1)) <= 0
}

func validSessionAcquisitionSource(s sessionAcquisitionSource, h prepareHistorical) bool {
	for _, value := range []*string{s.Config, s.Policy, s.Binary, s.Payload} {
		if value == nil || !digestPattern.MatchString(*value) {
			return false
		}
	}
	if s.Boot == nil || *s.Boot != h.Boot || s.Invocation == nil || *s.Invocation != h.ControllerInvocation || s.Credential == nil || *s.Credential != 1 || s.Name == nil || *s.Name != "nucleiron" || s.Endpoint == nil || *s.Endpoint == "" || s.Updated == nil || *s.Updated == "" || s.Repository == nil || *s.Repository != h.RepositoryID || s.Owner == nil || *s.Owner != "silence-force" || len(s.Service) != 6 {
		return false
	}
	service := map[string]string{}
	for _, name := range []string{"ActiveState", "SubState", "InvocationID", "MainPID", "User", "Group"} {
		raw := s.Service[name]
		var value string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
			return false
		}
		service[name] = value
	}
	pid, err := strconv.ParseUint(service["MainPID"], 10, 32)
	return err == nil && pid > 0 && service["ActiveState"] == "active" && service["SubState"] == "running" && service["InvocationID"] == *s.Invocation && service["User"] == "nucleiron" && (service["Group"] == "" || service["Group"] == "nucleiron")
}

// This is an expiry projection of a protected original, not signature
// verification or a claim that GitHub has observed revocation.
func expiredSessionAppToken(token string, expiry, app int64) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] == "" {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	defer clear(raw)
	var claims map[string]json.RawMessage
	if err != nil || unambiguous(raw) != nil || json.Unmarshal(raw, &claims) != nil || claims == nil {
		return false
	}
	var expires, issued int64
	if json.Unmarshal(claims["exp"], &expires) != nil || expires != expiry || json.Unmarshal(claims["iat"], &issued) != nil || issued <= 0 || issued >= expiry {
		return false
	}
	var number int64
	if json.Unmarshal(claims["iss"], &number) == nil {
		return number == app
	}
	var text string
	return json.Unmarshal(claims["iss"], &text) == nil && text == strconv.FormatInt(app, 10)
}
