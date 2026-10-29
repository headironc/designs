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

func runObserver(ctx context.Context, args []string, retire bool, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"github_observer_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	f := flag.NewFlagSet("github-observer", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	request := f.String("request", "", "protected original observer request")
	issuer := ""
	if retire {
		f.StringVar(&issuer, "issuer-sha256", "", "trusted original observer producer")
	}
	if f.Parse(args) != nil || f.NArg() != 0 || *request == "" || retire && !digestPattern.MatchString(issuer) {
		return fail()
	}
	stage = "identity"
	host, err := os.Hostname()
	if err != nil || host != "dev" || os.Getuid() != 0 || os.Geteuid() != 0 {
		return fail()
	}
	stage = "environment"
	if !controlledEnvironment(os.Environ()) {
		return fail()
	}
	stage = "secret_process"
	if protectSessionSecrets() != nil {
		return fail()
	}
	files := protectedFiles{uid: 0, boundary: "/"}
	stage = "request"
	raw, err := files.read(*request, 0600, 64*1024)
	if err != nil {
		return fail()
	}
	defer clear(raw)
	p, err := decodeObserverPlan(raw)
	if err != nil {
		return fail()
	}
	stage = "producer"
	producer, err := runningDigest()
	if err != nil {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	stage = "issuance"
	var out sessionOutcome
	if retire {
		stage = "retirement"
		out, err = (observerRetirer{files: files, parent: sessionParent, issuer: issuer, producer: producer, now: time.Now}).execute(ctx, raw)
	} else {
		out, err = (observerIssuer{files: files, parent: sessionParent, producer: producer, now: time.Now, admit: admitObserver}).execute(ctx, raw)
	}
	if err != nil {
		return fail()
	}
	stage = "output"
	if json.NewEncoder(output).Encode(struct {
		Operation string `json:"operation_id"`
		sessionOutcome
	}{p.Operation, out}) != nil {
		return fail()
	}
	return 0
}
