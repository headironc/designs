package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func lazyAdmission(files protectedFiles, githubPath, garmPath string) func(context.Context, receipt) (string, error) {
	return func(ctx context.Context, r receipt) (string, error) {
		a, err := files.read(githubPath, 0600, 64*1024+1)
		if err != nil {
			return "", errRefused
		}
		b, err := files.read(garmPath, 0600, 64*1024+1)
		if err != nil {
			return "", errRefused
		}
		gh, ga := strings.TrimSpace(string(a)), strings.TrimSpace(string(b))
		if !validToken(gh) || !validToken(ga) {
			return "", errRefused
		}
		api := newPlatforms(gh, ga, nil)
		defer api.close()
		return api.pauseAdmission(ctx, r)
	}
}

func runGateway(ctx context.Context, args []string, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"gateway_stop_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	f := flag.NewFlagSet("stop-gateway", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	requestPath := f.String("request", "", "original trusted request")
	operations := f.String("operations", "", "original operations directory")
	captureSHA := f.String("capture-sha256", "", "trusted original capture producer")
	pauseSHA := f.String("pause-sha256", "", "trusted original pause producer")
	gh := f.String("github-token-file", "", "existing credential for a first request")
	ga := f.String("garm-token-file", "", "existing credential for a first request")
	if f.Parse(args) != nil || f.NArg() != 0 || *requestPath == "" || *operations == "" || !digestPattern.MatchString(*captureSHA) || !digestPattern.MatchString(*pauseSHA) {
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
	files := protectedFiles{uid: 0, boundary: "/"}
	stage = "original"
	raw, err := files.read(*requestPath, 0600, 64*1024)
	if err != nil {
		return fail()
	}
	r, err := parseRequest(raw)
	if err != nil {
		return fail()
	}
	var original receipt
	var executionSHA string
	ok, err := files.originalExecution(*operations, r, raw, *captureSHA, &original, &executionSHA)
	if err != nil || !ok {
		return fail()
	}
	producer, err := runningDigest()
	if err != nil {
		return fail()
	}
	ph := pauseHost{proc: "/proc", cgroup: "/sys/fs/cgroup", realFS: true}
	p := pauser{files: files, parent: pauseParent, producer: *pauseSHA, boot: ph.boot, connect: func(c context.Context) (pauseConnection, error) {
		return connectPause(c, "/run/dbus/system_bus_socket", ph)
	}}
	h := gatewayHost{proc: "/proc", cgroup: "/sys/fs/cgroup", realFS: true, nginx: "/usr/sbin/nginx", controllerExe: "/opt/nucleus-halo/bin/garm"}
	g := gatewayStopper{pause: p, producer: producer, admission: lazyAdmission(files, *gh, *ga), connect: func(c context.Context) (gatewayConnection, error) {
		return connectGateway(c, "/run/dbus/system_bus_socket", h)
	}}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stage = "original_gateway_stop"
	result, err := g.execute(ctx, original, executionSHA)
	if err != nil {
		if result.Code == "" {
			switch {
			case ctx.Err() == context.Canceled || errors.Is(err, context.Canceled):
				result.Code = "gateway_observation_cancelled"
			case ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded):
				result.Code = "gateway_observation_expired"
			default:
				result.Code = "gateway_observation_refused"
			}
		}
		_ = json.NewEncoder(diagnostic).Encode(struct {
			Event       string         `json:"event"`
			Stage       string         `json:"stage"`
			Observation gatewayOutcome `json:"observation"`
		}{"gateway_stop_refused", stage, result})
		return 1
	}
	stage = "output"
	data, _ := json.Marshal(result)
	data = append(data, '\n')
	if n, e := output.Write(data); e != nil || n != len(data) {
		return fail()
	}
	return 0
}
