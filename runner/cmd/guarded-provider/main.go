//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/headironc/designs/runner/internal/provider"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--watchdog-check" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		os.Exit(provider.CheckWatchdog(ctx, os.Stderr))
	}
	if len(os.Args) != 2 || os.Args[1] != "--check-startup" {
		fmt.Fprintln(os.Stderr, `{"event":"guard_startup_refused","stage":"arguments"}`)
		os.Exit(1)
	}
	os.Exit(provider.CheckStartup(os.Stderr))
}
