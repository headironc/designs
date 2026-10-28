package lifecycle

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func protectSessionSecrets() error {
	if unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}) != nil || unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) != nil {
		return errRefused
	}
	var limit unix.Rlimit
	dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || dumpable != 0 || unix.Getrlimit(unix.RLIMIT_CORE, &limit) != nil || limit.Cur != 0 || limit.Max != 0 {
		return errRefused
	}
	return nil
}

func runSession(ctx context.Context, args []string, retire bool, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"garm_session_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	name := "acquire-garm-session"
	if retire {
		name = "retire-garm-session"
	}
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	request := f.String("request", "", "protected original credential-window request")
	var issuer string
	if retire {
		f.StringVar(&issuer, "issuer-sha256", "", "trusted original session producer")
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
	defer clear(raw)
	if err != nil {
		return fail()
	}
	p, err := decodeSessionPlan(raw)
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
	stage = "acquisition"
	var outcome sessionOutcome
	if retire {
		stage = "retirement"
		var bus *sessionBus
		defer func() {
			if bus != nil {
				bus.close()
			}
		}()
		r := sessionRetirer{files: files, parent: sessionParent, issuer: issuer, producer: producer, now: time.Now,
			check: func(c context.Context, plan sessionPlan, issued sessionIntent, ready sessionWindowReady) error {
				if bus == nil {
					var err error
					bus, err = connectSessionBus(c)
					if err != nil {
						return errRefused
					}
				}
				return bus.retirementTarget(c, plan, issued, ready)
			},
		}
		outcome, err = r.execute(ctx, raw)
	} else {
		a := sessionAcquirer{files: files, parent: sessionParent, producer: producer, now: time.Now, admit: admitSessionWindow}
		outcome, err = a.execute(ctx, raw)
	}
	if err != nil {
		return fail()
	}
	stage = "output"
	result := struct {
		Operation string `json:"operation_id"`
		sessionOutcome
	}{p.Operation, outcome}
	if json.NewEncoder(output).Encode(result) != nil {
		return fail()
	}
	return 0
}
