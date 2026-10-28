package lifecycle

import (
	"bytes"
	"encoding/json"
	"math/big"
	"reflect"
	"strconv"
	"strings"
)

const prepareOperation = "session-20261027-first-connection"
const prepareAmendment = "credential-rebind-175824179"
const prepareApproval = "01a17dad-af6b-7b41-a2a4-df0a16a24aa1"
const prepareReleaseSHA = "a50bf88764f85824e420e7f6ea46224493624ebf66f3c2e753a5b64681201f36"
const preparePlanSHA = "63486c3d0ad1075ef3f075bc4cdfc41fe14f69c66fd262f8ab1e3f497e4e13df"
const prepareBootstrapSHA = "9271a4154f0bf742129efb630fb4453f7ced06a6e65e1978eccc68cff2b701c1"
const prepareController = "7c78121a-33f1-41bd-a3a2-d187cb00b590"
const prepareOldRepository = "6f1b8cfe-ec05-49a6-a4f5-8dcf2866899d"
const preparePublicKey = "6a33147cba31aad74310aa9329a89fa0331c1c2def87dabc95593bf0f26b900b"

var prepareMembers = []string{"acquire-session.py", "adapters/recover-controller-dependencies.py", "adapters/verify-controller-start.py", "credential", "entry.py", "failure-evidence.py", "first-connection.py", "host.py", "install-candidate.py", "install-services.py", "lifecycle.py", "rebind.py", "records_only.py", "request.json"}
var prepareFixedPaths = []string{
	"/etc/nucleiron-egress/dns.yaml", "/etc/nucleiron-egress/squid.conf", "/etc/nucleiron-gateway/gateway.conf", "/etc/nucleiron-gateway/tls/ca.crt", "/etc/nucleiron-gateway/tls/server.crt", "/etc/nucleiron-gateway/tls/server.key", "/etc/nucleus-halo/garm-provider-lxd.toml", "/etc/nucleus-halo/garm.toml", "/etc/nucleus-halo/provider-policy.json", "/etc/rsyslog.d/nucleiron/server.conf",
	"/etc/systemd/system/nucleiron-dns.service", "/etc/systemd/system/nucleiron-egress.service", "/etc/systemd/system/nucleiron-firewall.service", "/etc/systemd/system/nucleiron-gateway.service", "/etc/systemd/system/nucleiron-logs.service", "/etc/systemd/system/nucleiron-provider-watchdog.service", "/etc/systemd/system/nucleiron-provider-watchdog.timer", "/etc/systemd/system/nucleiron.service", "/etc/systemd/system/nucleiron.service.d/30-provider-guard.conf", "/home/headiron/runner-deployment/halo-20261026/nucleiron-ca.crt", "/opt/nucleus-halo/bin/garm", "/opt/nucleus-halo/bin/garm-provider-lxd", "/opt/nucleus-halo/bin/guarded-lxd-provider", "/opt/nucleus-halo/bin/network-firewall.py", "/usr/sbin/nginx",
}
var prepareUnits = []string{controllerUnit, gatewayUnit, pauseWorker, pauseTimer, "nucleiron-logs.service", firewallUnit, "nucleiron-dns.service", "nucleiron-egress.service"}

type prepareFact struct {
	SHA    *string `json:"sha256"`
	Size   *int64  `json:"size"`
	UID    *uint32 `json:"uid"`
	GID    *uint32 `json:"gid"`
	Mode   *uint32 `json:"mode"`
	Xattrs *string `json:"xattrs_sha256"`
}

func validPrepareFacts(facts map[string]prepareFact) bool {
	if len(facts) != len(prepareFixedPaths) {
		return false
	}
	for _, path := range prepareFixedPaths {
		f, ok := facts[path]
		if !ok || f.SHA == nil || !digestPattern.MatchString(*f.SHA) || f.Size == nil || *f.Size < 0 || f.UID == nil || *f.UID != 0 || f.GID == nil || f.Mode == nil || *f.Mode > 07777 || f.Xattrs == nil || !digestPattern.MatchString(*f.Xattrs) {
			return false
		}
	}
	return true
}

type preparePlan struct {
	Operation     *string                `json:"operation"`
	Amendment     *string                `json:"amendment"`
	Approval      *string                `json:"approval_message"`
	Boot          *string                `json:"boot_id"`
	Controller    *string                `json:"controller_id"`
	OldRepository *string                `json:"old_repository_id"`
	RepositoryID  *int64                 `json:"repository_id"`
	FullName      *string                `json:"full_name"`
	Installation  *int64                 `json:"installation_id"`
	Expires       *int64                 `json:"expires_at"`
	Files         map[string]prepareFact `json:"fixed_files"`
}

func decodePreparePlan(raw []byte) (*preparePlan, error) {
	var p preparePlan
	if strictObject(raw, &p) != nil || p.Operation == nil || *p.Operation != prepareOperation || p.Amendment == nil || *p.Amendment != prepareAmendment || p.Approval == nil || *p.Approval != prepareApproval || p.Boot == nil || !uuidPattern.MatchString(*p.Boot) || p.Controller == nil || *p.Controller != prepareController || p.OldRepository == nil || *p.OldRepository != prepareOldRepository || p.RepositoryID == nil || *p.RepositoryID != 1432619931 || p.FullName == nil || *p.FullName != "headironc/nucleus" || p.Installation == nil || *p.Installation != 175824179 || p.Expires == nil || *p.Expires <= 0 || !validPrepareFacts(p.Files) {
		return nil, errRefused
	}
	return &p, nil
}

type prepareManifestRow struct {
	Path *string `json:"path"`
	SHA  *string `json:"sha256"`
	Size *int64  `json:"size"`
}
type prepareManifest struct {
	Approval *string              `json:"approval_message"`
	Files    []prepareManifestRow `json:"files"`
}

func decodePrepareManifest(raw []byte) (map[string]int64, error) {
	var m prepareManifest
	if strictObject(raw, &m) != nil || m.Approval == nil || *m.Approval != prepareApproval || len(m.Files) != len(prepareMembers) {
		return nil, errRefused
	}
	out := map[string]int64{}
	for _, r := range m.Files {
		if r.Path == nil || r.SHA == nil || !digestPattern.MatchString(*r.SHA) || r.Size == nil || *r.Size <= 0 || *r.Size > 32*1024*1024 {
			return nil, errRefused
		}
		if _, seen := out[*r.Path]; seen {
			return nil, errRefused
		}
		out[*r.Path] = *r.Size
	}
	for _, name := range prepareMembers {
		if _, ok := out[name]; !ok {
			return nil, errRefused
		}
	}
	return out, nil
}

type prepareIntent struct {
	Operation *string `json:"operation"`
	Amendment *string `json:"amendment"`
	Approval  *string `json:"approval_message"`
}
type prepareRepository struct {
	ID    *string `json:"id"`
	Owner *string `json:"owner"`
	Name  *string `json:"name"`
}
type prepareBefore struct {
	Payload        json.RawMessage        `json:"payload"`
	AppID          *int64                 `json:"app_id"`
	Installation   *int64                 `json:"installation_id"`
	PublicKey      *string                `json:"public_key_sha256"`
	TTL            *json.Number           `json:"garm_ttl_seconds"`
	Repository     *prepareRepository     `json:"repository"`
	RepositoryRow  []json.RawMessage      `json:"repository_row"`
	CredentialRow  []json.RawMessage      `json:"credential_row"`
	CredentialName *string                `json:"credential_name"`
	Webhook        json.RawMessage        `json:"webhook"`
	Files          map[string]prepareFact `json:"files"`
	Invocation     *string                `json:"controller_invocation"`
}
type prepareBinding struct {
	Boot   *string                `json:"boot_id"`
	Before map[string]prepareFact `json:"before"`
	After  map[string]prepareFact `json:"after"`
}
type prepareHost struct {
	Files map[string]prepareFact `json:"files"`
}
type preparePrestate struct {
	Services map[string]json.RawMessage `json:"services"`
	Host     *prepareHost               `json:"host"`
}
type prepareComplete struct {
	BeforeSHA *string `json:"before_sha256"`
}

func prepareString(raw json.RawMessage) bool { return len(raw) > 0 && raw[0] == '"' }
func prepareScalar(raw json.RawMessage) bool { return len(raw) > 0 && raw[0] != '{' && raw[0] != '[' }
func prepareRowValue(raw json.RawMessage, expected any) bool {
	encoded, err := json.Marshal(expected)
	return err == nil && bytes.Equal(raw, encoded)
}
func decodePrepareBefore(raw []byte) (*prepareBefore, error) {
	var b prepareBefore
	if strictObject(raw, &b) != nil || !prepareString(b.Payload) || !prepareString(b.Webhook) || b.AppID == nil || *b.AppID != 5469697 || b.Installation == nil || *b.Installation != 175207088 || b.PublicKey == nil || *b.PublicKey != preparePublicKey || b.CredentialName == nil || *b.CredentialName != "nucleiron" || b.Invocation == nil || !pauseHex.MatchString(*b.Invocation) || !validPrepareFacts(b.Files) || b.TTL == nil || len(b.TTL.String()) > 128 {
		return nil, errRefused
	}
	// The producer emits time.Duration.Seconds(), bounded by an int64 number
	// of nanoseconds. Reject huge exponents before constructing a rational.
	seconds, err := strconv.ParseFloat(b.TTL.String(), 64)
	if err != nil || seconds < 86400 || seconds > 9223372037 {
		return nil, errRefused
	}
	ttl, ok := new(big.Rat).SetString(b.TTL.String())
	if !ok || ttl.Cmp(big.NewRat(86400, 1)) < 0 {
		return nil, errRefused
	}
	r := b.Repository
	if r == nil || r.ID == nil || *r.ID != prepareOldRepository || r.Owner == nil || *r.Owner != "silence-force" || r.Name == nil || *r.Name != "nucleus" || len(b.RepositoryRow) != 7 || len(b.CredentialRow) != 13 {
		return nil, errRefused
	}
	for _, row := range [][]json.RawMessage{b.RepositoryRow, b.CredentialRow} {
		for _, v := range row {
			if !prepareScalar(v) {
				return nil, errRefused
			}
		}
	}
	for i, v := range []any{prepareOldRepository, "silence-force", "nucleus", 1} {
		if !prepareRowValue(b.RepositoryRow[i], v) {
			return nil, errRefused
		}
	}
	for i, v := range []any{1, "nucleiron", "app"} {
		if !prepareRowValue(b.CredentialRow[i], v) {
			return nil, errRefused
		}
	}
	var endpoint, repositoryEndpoint string
	if json.Unmarshal(b.CredentialRow[5], &endpoint) != nil || endpoint == "" || len(endpoint) > 100 || json.Unmarshal(b.RepositoryRow[4], &repositoryEndpoint) != nil || repositoryEndpoint != endpoint || !prepareRowValue(b.RepositoryRow[5], 0) || !prepareString(b.RepositoryRow[6]) || !prepareString(b.CredentialRow[3]) {
		return nil, errRefused
	}
	return &b, nil
}

type prepareJob struct {
	ID   *uint32 `json:"id"`
	Unit *string `json:"unit"`
	Path *string `json:"path"`
}
type prepareUnit struct {
	Active     *string      `json:"active"`
	Sub        *string      `json:"sub"`
	Job        *uint32      `json:"job_id"`
	Jobs       []prepareJob `json:"jobs"`
	Invocation *string      `json:"invocation"`
}
type prepareService struct {
	prepareUnit
	Main     *uint32  `json:"main_pid"`
	Control  *uint32  `json:"control_pid"`
	PIDs     []uint32 `json:"pids"`
	Result   *string  `json:"result"`
	Restarts *uint32  `json:"restarts"`
	Start    *uint64  `json:"start_us"`
	Exit     *uint64  `json:"exit_us"`
	Status   *int32   `json:"exit_status"`
	Code     *int32   `json:"exit_code"`
}
type prepareUnitProjection struct {
	Active     string `json:"active"`
	Invocation string `json:"invocation"`
	Job        uint32 `json:"job_id"`
	Jobs       int    `json:"jobs_count"`
	Main       uint32 `json:"main_pid"`
	Control    uint32 `json:"control_pid"`
	PIDs       int    `json:"pids_count"`
}

func validPrepareUnit(u prepareUnit, name string) bool {
	if u.Active == nil || u.Sub == nil || *u.Sub == "" || len(*u.Sub) > 64 || u.Job == nil || u.Jobs == nil || len(u.Jobs) > 4096 || u.Invocation == nil || *u.Invocation != "" && !pauseHex.MatchString(*u.Invocation) {
		return false
	}
	if *u.Active != "active" && *u.Active != "activating" && *u.Active != "inactive" {
		return false
	}
	seen := map[uint32]bool{}
	for _, j := range u.Jobs {
		if j.ID == nil || *j.ID == 0 || seen[*j.ID] || j.Unit == nil || *j.Unit != name || j.Path == nil || *j.Path != pauseJobPath(*j.ID) {
			return false
		}
		seen[*j.ID] = true
	}
	return true
}
func decodePrepareServices(raw map[string]json.RawMessage) (map[string]prepareUnitProjection, error) {
	if len(raw) != len(prepareUnits) {
		return nil, errRefused
	}
	out := map[string]prepareUnitProjection{}
	for _, name := range prepareUnits {
		body, ok := raw[name]
		if !ok {
			return nil, errRefused
		}
		var unit prepareUnit
		var s prepareService
		if name == pauseTimer {
			if strictObject(body, &unit) != nil {
				return nil, errRefused
			}
		} else {
			if strictObject(body, &s) != nil || s.Main == nil || *s.Main > 2147483647 || s.Control == nil || *s.Control > 2147483647 || s.PIDs == nil || len(s.PIDs) > 4096 || s.Result == nil || !knownServiceResult(*s.Result) || s.Restarts == nil || s.Start == nil || s.Exit == nil || s.Status == nil || s.Code == nil {
				return nil, errRefused
			}
			unit = s.prepareUnit
			seen := map[uint32]bool{}
			for _, pid := range s.PIDs {
				if pid == 0 || pid > 2147483647 || seen[pid] {
					return nil, errRefused
				}
				seen[pid] = true
			}
		}
		if !validPrepareUnit(unit, name) {
			return nil, errRefused
		}
		idle := *unit.Job == 0 && len(unit.Jobs) == 0
		inactive := *unit.Active == "inactive" && idle && (name == pauseTimer || *s.Main == 0 && *s.Control == 0 && len(s.PIDs) == 0)
		running := *unit.Active == "active" && idle
		if name != pauseTimer {
			found := false
			for _, pid := range s.PIDs {
				found = found || pid == *s.Main
			}
			running = running && *unit.Sub == "running" && *s.Main > 1 && found && *s.Control == 0
		}
		if name != pauseWorker && !(name == controllerUnit || name == gatewayUnit || name == pauseTimer) && !running || (name == controllerUnit || name == gatewayUnit || name == pauseTimer) && !running && !inactive {
			return nil, errRefused
		}
		v := prepareUnitProjection{Active: *unit.Active, Invocation: *unit.Invocation, Job: *unit.Job, Jobs: len(unit.Jobs)}
		if name != pauseTimer {
			v.Main, v.Control, v.PIDs = *s.Main, *s.Control, len(s.PIDs)
		}
		out[name] = v
	}
	if out[gatewayUnit].Active == "active" && out[controllerUnit].Active != "active" {
		return nil, errRefused
	}
	return out, nil
}

func prepareEqualFacts(a, b map[string]prepareFact) bool {
	return validPrepareFacts(a) && validPrepareFacts(b) && reflect.DeepEqual(a, b)
}
func prepareReceiptBytes(raw []byte) bool {
	return len(raw) > 1 && raw[len(raw)-1] == '\n' && !strings.HasSuffix(string(raw[:len(raw)-1]), "\n")
}
