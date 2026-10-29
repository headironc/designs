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

func runConnection(ctx context.Context, args []string, retire bool, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"disabled_connection_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	f := flag.NewFlagSet("disabled-connection", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	request := f.String("request", "", "protected trusted original connection plan")
	webhook := ""
	if !retire {
		f.StringVar(&webhook, "webhook-secret-file", "", "protected original webhook secret for repository creation")
	}
	if f.Parse(args) != nil || f.NArg() != 0 || *request == "" {
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
	stage = "request"
	files := protectedFiles{uid: 0, boundary: "/"}
	raw, err := files.read(*request, 0600, 64*1024)
	if err != nil {
		return fail()
	}
	defer clear(raw)
	p, err := decodeConnectionPlan(raw)
	if err != nil || !retire && p.RepositoryMode == "create" && webhook == "" {
		return fail()
	}
	stage = "producer"
	producer, err := runningDigest()
	if err != nil {
		return fail()
	}
	e := connectionExecutor{files: files, parent: sessionParent, producer: producer, now: time.Now, admit: admitConnection(webhook), recoverStop: recoverConnectionStop}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var result any
	if retire {
		stage = "credential_retirement"
		state, err := e.retire(ctx, raw)
		if err != nil {
			return fail()
		}
		result = state
	} else {
		binding, current, err := e.execute(ctx, raw)
		stage = current
		if err != nil {
			return fail()
		}
		result = struct {
			State        string            `json:"state"`
			Binding      connectionBinding `json:"binding"`
			EnablePermit bool              `json:"enable_permit"`
		}{current, binding, false}
	}
	stage = "output"
	if json.NewEncoder(output).Encode(result) != nil {
		return fail()
	}
	return 0
}
