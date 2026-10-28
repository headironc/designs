package lifecycle

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSessionEntrypointsRejectAlternativeInputsWithoutEcho(t *testing.T) {
	for _, command := range []string{"acquire-garm-session", "retire-garm-session"} {
		for _, args := range [][]string{
			{},
			{"--password", "fixture-sensitive-password"},
			{"--url", "https://fixture-sensitive.invalid"},
			{"--request", "/fixture-sensitive-request", "extra"},
			{"--request", "/fixture-sensitive-request", "--issuer-sha256", "fixture-sensitive-issuer"},
		} {
			var output, diagnostic bytes.Buffer
			if Run(context.Background(), append([]string{command}, args...), &output, &diagnostic) != 1 || output.Len() != 0 || strings.Contains(diagnostic.String(), "fixture-sensitive") || !strings.Contains(diagnostic.String(), `"event":"garm_session_refused"`) {
				t.Fatal("session argument boundary", command, output.String(), diagnostic.String())
			}
		}
	}
}

func TestSessionSecretProcessInChild(t *testing.T) {
	if os.Getenv("RUNNER_SESSION_CORE_CHILD") == "1" {
		if protectSessionSecrets() != nil {
			os.Exit(2)
		}
		var limit unix.Rlimit
		dumpable, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		if err != nil || dumpable != 0 || unix.Getrlimit(unix.RLIMIT_CORE, &limit) != nil || limit.Cur != 0 || limit.Max != 0 {
			os.Exit(3)
		}
		os.Stdout.WriteString("protected\n")
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSessionSecretProcessInChild$")
	cmd.Env = append(os.Environ(), "RUNNER_SESSION_CORE_CHILD=1")
	result, err := cmd.CombinedOutput()
	if err != nil || string(result) != "protected\n" {
		t.Fatal("secret process protection", err, string(result))
	}
}
