package lifecycle

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const gatewayUnit = "nucleiron-gateway.service"
const gatewayGroup = "/system.slice/" + gatewayUnit
const controllerUnit = "nucleiron.service"
const firewallUnit = "nucleiron-firewall.service"

type serviceProcess struct {
	PID              uint32 `json:"pid"`
	Start            uint64 `json:"starttime_ticks"`
	UID              uint32 `json:"uid"`
	GID              uint32 `json:"gid"`
	ExecutableDevice uint64 `json:"executable_device"`
	ExecutableInode  uint64 `json:"executable_inode"`
	ExecutableSHA    string `json:"executable_sha256"`
}

type gatewayFacts struct {
	Active        string         `json:"active"`
	Sub           string         `json:"sub"`
	Invocation    string         `json:"invocation"`
	StartUSec     uint64         `json:"start_usec"`
	Main          serviceProcess `json:"main"`
	Control       serviceProcess `json:"control"`
	Group         string         `json:"cgroup"`
	GroupDevice   uint64         `json:"cgroup_device"`
	GroupInode    uint64         `json:"cgroup_inode"`
	Job           uint32         `json:"job"`
	Jobs          []uint32       `json:"jobs"`
	Result        string         `json:"result"`
	Configuration string         `json:"configuration_sha256"`
}

type controllerFacts struct {
	Invocation    string         `json:"invocation"`
	StartUSec     uint64         `json:"start_usec"`
	Main          serviceProcess `json:"main"`
	Configuration string         `json:"configuration_sha256"`
}

type gatewayIntent struct {
	Version        int             `json:"version"`
	Operation      string          `json:"operation_id"`
	ExecutionSHA   string          `json:"execution_sha256"`
	CaptureSHA     string          `json:"capture_producer_sha256"`
	PauseSHA       string          `json:"pause_producer_sha256"`
	PauseIntentSHA string          `json:"pause_intent_sha256"`
	PauseDrainSHA  string          `json:"pause_drain_sha256"`
	ProducerSHA    string          `json:"gateway_producer_sha256"`
	Boot           string          `json:"boot_id"`
	Manager        string          `json:"manager"`
	Unit           string          `json:"unit"`
	AdmissionSHA   string          `json:"admission_sha256"`
	Before         gatewayFacts    `json:"before"`
	Controller     controllerFacts `json:"controller"`
}

type gatewayDrain struct {
	IntentSHA       string          `json:"intent_sha256"`
	Job             uint32          `json:"job"`
	Facts           gatewayFacts    `json:"facts"`
	ControllerAfter controllerFacts `json:"controller_after"`
}

type gatewayRecords struct {
	Intent    gatewayIntent
	IntentSHA string
	Job       *pauseJob
	Event     *pauseEvent
	Drain     *gatewayDrain
}

type gatewayStore struct{ *serviceFiles }

func (s *gatewayStore) noWatchdogStop() error {
	var st unix.Stat_t
	err := unix.Fstatat(int(s.parent.Fd()), "stop-request.json", &st, unix.AT_SYMLINK_NOFOLLOW)
	if err != unix.ENOENT || !s.unchanged() {
		return errRefused
	}
	return nil
}

func bytesSHA(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func (s *gatewayStore) load(r receipt, executionSHA, producer string, pause *pauseView) (gatewayRecords, error) {
	out := gatewayRecords{}
	if s.dir == nil || !s.unchanged() || !pause.store.unchanged() {
		return out, errRefused
	}
	entries, err := os.ReadDir(s.path)
	if err != nil || len(entries) > 4 {
		return out, errRefused
	}
	present := map[string]bool{}
	for _, entry := range entries {
		switch entry.Name() {
		case "intent.json", "job.json", "event.json", "drain.json":
			present[entry.Name()] = true
		default:
			return out, errRefused
		}
	}
	if !present["intent.json"] || present["event.json"] && !present["job.json"] || present["drain.json"] && !present["event.json"] {
		return out, errRefused
	}
	read := func(name string, target any) ([]byte, error) {
		raw, e := s.files.readFile(filepath.Join(s.path, name), 0600, 64*1024, true)
		if e != nil || decodePauseRecord(raw, target) != nil || s.track(name, raw) != nil {
			return nil, errRefused
		}
		return raw, nil
	}
	raw, err := read("intent.json", &out.Intent)
	i := out.Intent
	if err != nil || i.Version != 1 || i.Operation != r.Expected.Operation || i.ExecutionSHA != executionSHA || i.CaptureSHA != r.ProducerSHA || !digestPattern.MatchString(producer) || i.ProducerSHA != producer || i.PauseSHA != pause.producer || i.PauseIntentSHA != pause.saved.IntentSHA || i.PauseDrainSHA != bytesSHA(pause.store.raw["drain.json"]) || pause.saved.Drain == nil || i.Boot != r.Observed.Host.BootID || i.Boot != pause.saved.Intent.Boot || i.Manager != pause.saved.Intent.Manager || managerOwner(i.Manager) == "" || i.Unit != gatewayUnit || !digestPattern.MatchString(i.AdmissionSHA) || !gatewayInitial(i.Before) || !validController(i.Controller) {
		return out, errRefused
	}
	out.IntentSHA = bytesSHA(raw)
	if present["job.json"] {
		out.Job = &pauseJob{}
		if _, err = read("job.json", out.Job); err != nil || out.Job.IntentSHA != out.IntentSHA || out.Job.ID == 0 || out.Job.Path != pauseJobPath(out.Job.ID) {
			return out, errRefused
		}
	}
	if present["event.json"] {
		out.Event = &pauseEvent{}
		if _, err = read("event.json", out.Event); err != nil || out.Event.IntentSHA != out.IntentSHA || out.Event.ID != out.Job.ID || out.Event.Path != out.Job.Path || out.Event.Unit != gatewayUnit || out.Event.Sender != managerOwner(i.Manager) || !pauseJobResult(out.Event.Result) {
			return out, errRefused
		}
	}
	if present["drain.json"] {
		out.Drain = &gatewayDrain{}
		if _, err = read("drain.json", out.Drain); err != nil || out.Drain.IntentSHA != out.IntentSHA || out.Drain.Job != out.Job.ID || out.Event.Result != "done" || !gatewayDrained(out.Drain.Facts) || gatewayCompatible(i.Before, out.Drain.Facts) != nil || out.Drain.ControllerAfter != i.Controller {
			return out, errRefused
		}
	}
	if s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil || !s.unchanged() || !pause.store.unchanged() {
		return out, errRefused
	}
	return out, nil
}

type gatewayConnection interface {
	close()
	identity(context.Context) (string, error)
	snapshot(context.Context, *gatewayFacts) (gatewayFacts, error)
	controller(context.Context) (controllerFacts, error)
	stopGateway(context.Context) (pauseJob, error)
	event(context.Context, pauseJob) (pauseEvent, error)
	drained(context.Context, gatewayFacts, gatewayFacts) error
}
