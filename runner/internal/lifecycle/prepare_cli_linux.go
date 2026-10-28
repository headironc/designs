package lifecycle

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func runPrepare(ctx context.Context, args []string, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"prepare_observation_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	f := flag.NewFlagSet("inspect-prepare", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	release := f.String("release-sha256", "", "exact historical interpretation manifest")
	plan := f.String("plan-sha256", "", "exact historical host plan")
	if f.Parse(args) != nil || f.NArg() != 0 || *release != prepareReleaseSHA || *plan != preparePlanSHA {
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
	stage = "producer"
	producer, err := runningDigest()
	if err != nil {
		return fail()
	}
	p := prepareReader{files: protectedFiles{uid: 0, boundary: "/"}, parent: "/var/lib/nucleus-halo-first-connection-inputs/" + prepareOperation, release: "/var/lib/nucleus-halo-rebind-code/" + prepareReleaseSHA, releaseSHA: prepareReleaseSHA, planSHA: preparePlanSHA}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	report := p.observe(ctx, producer)
	stage = "report"
	raw, err := json.Marshal(report)
	if err != nil || len(raw) > 128*1024 {
		return fail()
	}
	raw = append(raw, '\n')
	if n, e := output.Write(raw); e != nil || n != len(raw) {
		return fail()
	}
	return 0
}
