package lifecycle

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/headironc/designs/runner/internal/provider"
)

type resourceObservation struct {
	State    string `json:"state"`
	Code     string `json:"code,omitempty"`
	Count    int    `json:"count"`
	identity string
}

func unknown(code string) resourceObservation {
	return resourceObservation{State: "unknown", Code: code}
}
func resource(state string, count int) resourceObservation {
	return resourceObservation{State: state, Count: count}
}

type jobObservation struct {
	State      string `json:"state"`
	RunState   string `json:"run_state,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Code       string `json:"code,omitempty"`
}
type garmObservation struct {
	resourceObservation
	TargetStatus string `json:"target_status,omitempty"`
	Admission    string `json:"admission,omitempty"`
}
type machineObservation struct {
	Instances  resourceObservation `json:"instances"`
	Volumes    resourceObservation `json:"volumes"`
	Operations resourceObservation `json:"operations"`
}
type hostObservation struct {
	Boot    resourceObservation `json:"boot"`
	Process resourceObservation `json:"process"`
	Cgroup  resourceObservation `json:"cgroup"`
}
type guardObservation struct {
	resourceObservation
	Facts provider.GuardObservation `json:"facts"`
}
type diagnosticReport struct {
	Version            int                  `json:"version"`
	Operation          string               `json:"operation_id"`
	CaptureProducer    string               `json:"capture_producer_sha256"`
	DiagnosticProducer string               `json:"diagnostic_producer_sha256"`
	ObservedAt         string               `json:"observed_at"`
	Job                jobObservation       `json:"job"`
	Registration       resourceObservation  `json:"registration"`
	GARM               garmObservation      `json:"garm"`
	Machine            machineObservation   `json:"machine"`
	Host               hostObservation      `json:"host"`
	Guard              guardObservation     `json:"guard"`
	Services           []serviceObservation `json:"services"`
	Database           databaseObservation  `json:"database"`
	ProvenanceGaps     []string             `json:"provenance_gaps"`
	Closure            string               `json:"closure"`
	WatchdogPause      pauseOutcome         `json:"watchdog_pause"`
	GatewayStop        gatewayOutcome       `json:"gateway_stop"`
}

type diagnostician struct {
	job          func(context.Context, receipt) jobObservation
	registration func(context.Context, receipt) resourceObservation
	garm         func(context.Context, receipt) garmObservation
	machine      func(context.Context, receipt) machineObservation
	host         func(context.Context, receipt) hostObservation
	guard        func(context.Context) (provider.GuardObservation, error)
	services     func(context.Context) []serviceObservation
	database     func(context.Context, receipt) databaseObservation
	pause        func(context.Context) pauseOutcome
	gateway      func(context.Context) gatewayOutcome
}

func (d diagnostician) observe(ctx context.Context, r receipt, producer string) diagnosticReport {
	// Each source has a separate timeout. A failure does not hide independent
	// observations and no empty inventory grants missing historical provenance.
	out := diagnosticReport{Version: 1, Operation: r.Expected.Operation, CaptureProducer: r.ProducerSHA, DiagnosticProducer: producer, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Closure: "unknown", ProvenanceGaps: []string{"original_service_jobs_missing", "execution_credentials_closure_missing", "external_operation_receipts_missing", "original_controller_configuration_provenance_missing"}}
	observe := func(f func(context.Context)) {
		c, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		f(c)
	}
	observe(func(c context.Context) { out.Job = d.job(c, r) })
	observe(func(c context.Context) { out.Registration = d.registration(c, r) })
	observe(func(c context.Context) { out.GARM = d.garm(c, r) })
	observe(func(c context.Context) { out.Machine = d.machine(c, r) })
	observe(func(c context.Context) { out.Host = d.host(c, r) })
	observe(func(c context.Context) {
		v, err := d.guard(c)
		out.Guard = guardObservation{resourceObservation: resource("observed", 0), Facts: v}
		if err != nil {
			out.Guard.resourceObservation = unknown("guard_" + v.Stage)
		} else if v.Controller != r.Expected.Controller || v.Repository != r.Expected.GARMRepository || v.PolicySHA != r.Observed.Guard.PolicySHA || v.ScaleSet != r.Observed.Guard.ScaleSet || v.Pool != r.Observed.Guard.Pool || v.Image != r.Observed.Guard.Image || v.Name != "" && (v.Name != r.Observed.RunnerName || v.UUID != r.Observed.Guard.UUID || v.AgentID != r.Observed.RunnerID) {
			out.Guard.resourceObservation = resource("mismatch", 1)
		}
	})
	observe(func(c context.Context) { out.Services = d.services(c) })
	observe(func(c context.Context) { out.Database = d.database(c, r) })
	out.WatchdogPause = pauseOutcome{State: "unknown", Code: "pause_producer_untrusted"}
	if d.pause != nil {
		observe(func(c context.Context) { out.WatchdogPause = d.pause(c) })
	}
	out.GatewayStop = gatewayOutcome{State: "unknown", Code: "gateway_producer_untrusted"}
	if d.gateway != nil {
		observe(func(c context.Context) { out.GatewayStop = d.gateway(c) })
	}
	out.Database.RuntimeConnection = "runtime_database_connection_unknown"
	if out.Database.Configuration == "current_file_fixed_database_verified" {
		var after []serviceObservation
		observe(func(c context.Context) { after = d.services(c) })
		for _, service := range out.Services {
			if service.Unit == "nucleiron.service" && service.State == "observed" && service.Configuration == "current_process_config_path_verified" {
				for _, current := range after {
					if current.Unit == service.Unit && reflect.DeepEqual(current, service) {
						out.Database.Configuration = "current_file_and_process_config_path_verified"
					}
				}
			}
		}
	}
	return out
}

func runDiagnostic(ctx context.Context, args []string, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"execution_diagnostic_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	f := flag.NewFlagSet("diagnose-execution", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	requestPath := f.String("request", "", "original trusted request")
	operations := f.String("operations", "", "protected operations directory")
	captureSHA := f.String("capture-sha256", "", "trusted original producer digest")
	ghPath := f.String("github-token-file", "", "existing credential")
	gaPath := f.String("garm-token-file", "", "existing credential")
	pauseSHA := f.String("pause-sha256", "", "trusted original pause producer")
	gatewaySHA := f.String("gateway-sha256", "", "trusted original gateway stop producer")
	if f.Parse(args) != nil || f.NArg() != 0 || *requestPath == "" || *operations == "" || !digestPattern.MatchString(*captureSHA) || *ghPath == "" || *gaPath == "" {
		return fail()
	}
	if *pauseSHA != "" && !digestPattern.MatchString(*pauseSHA) {
		return fail()
	}
	if *gatewaySHA != "" && (!digestPattern.MatchString(*gatewaySHA) || !digestPattern.MatchString(*pauseSHA)) {
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
	p := protectedFiles{uid: 0, boundary: "/"}
	stage = "request"
	raw, err := p.read(*requestPath, 0600, 64*1024)
	if err != nil {
		return fail()
	}
	r, err := parseRequest(raw)
	if err != nil {
		return fail()
	}
	stage = "original"
	var original receipt
	var executionSHA string
	ok, err := p.originalExecution(*operations, r, raw, *captureSHA, &original, &executionSHA)
	if err != nil || !ok {
		return fail()
	}
	stage = "producer"
	producer, err := runningDigest()
	if err != nil {
		return fail()
	}
	stage = "credential"
	gh, err := p.read(*ghPath, 0600, 64*1024+1)
	if err != nil {
		return fail()
	}
	ga, err := p.read(*gaPath, 0600, 64*1024+1)
	if err != nil {
		return fail()
	}
	ghToken, gaToken := strings.TrimSpace(string(gh)), strings.TrimSpace(string(ga))
	if !validToken(ghToken) || !validToken(gaToken) {
		return fail()
	}
	platform := newPlatforms(ghToken, gaToken, nil)
	defer platform.close()
	d := diagnostician{job: platform.diagnoseJob, registration: platform.diagnoseRegistration, garm: platform.diagnoseGARM, machine: func(c context.Context, r receipt) machineObservation {
		return diagnoseMachine(c, "/var/snap/lxd/common/lxd/unix.socket", r)
	}, host: (hostFiles{proc: "/proc", cgroup: "/sys/fs/cgroup", realFS: true}).diagnose, guard: provider.ObserveGuard, services: observeServices, database: observeDatabase}
	if *pauseSHA != "" {
		h := pauseHost{proc: "/proc", cgroup: "/sys/fs/cgroup", realFS: true}
		observer := pauser{files: p, parent: pauseParent, producer: *pauseSHA, boot: h.boot, connect: func(c context.Context) (pauseConnection, error) {
			return connectPause(c, "/run/dbus/system_bus_socket", h)
		}}
		d.pause = func(c context.Context) pauseOutcome { return observer.observe(c, original, executionSHA) }
		if *gatewaySHA != "" {
			gh := gatewayHost{proc: "/proc", cgroup: "/sys/fs/cgroup", realFS: true, nginx: "/usr/sbin/nginx", controllerExe: "/opt/nucleus-halo/bin/garm"}
			gateway := gatewayStopper{pause: observer, producer: *gatewaySHA, connect: func(c context.Context) (gatewayConnection, error) {
				return connectGateway(c, "/run/dbus/system_bus_socket", gh)
			}}
			d.gateway = func(c context.Context) gatewayOutcome { return gateway.observe(c, original, executionSHA) }
		}
	}
	report := d.observe(ctx, original, producer)
	stage = "report"
	data, err := json.Marshal(report)
	if err != nil || len(data) > 128*1024 {
		return fail()
	}
	data = append(data, '\n')
	n, err := output.Write(data)
	if err != nil || n != len(data) {
		return fail()
	}
	return 0
}
