// runner-bootstrap consumes private provider input on stdin inside a fresh guest.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/headironc/designs/runner/internal/bootstrap"
)

func main() {
	if len(os.Args) != 1 {
		bootstrap.ReportInvalidInput(os.Stdout)
		os.Exit(1)
	}
	config, err := bootstrap.Decode(os.Stdin)
	if err != nil {
		bootstrap.ReportInvalidInput(os.Stdout)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bootstrap.Install(ctx, config, "/home/runner/actions-runner", "/etc/os-release", os.Stdout); err != nil {
		os.Exit(1)
	}
}
