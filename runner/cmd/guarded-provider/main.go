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
	if len(os.Args) == 1 {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		environment := make(map[string]string)
		for _, key := range []string{"GARM_COMMAND", "GARM_CONTROLLER_ID", "GARM_POOL_ID", "GARM_PROVIDER_CONFIG_FILE", "GARM_INTERFACE_VERSION"} {
			environment[key] = os.Getenv(key)
		}
		os.Exit(provider.Create(ctx, environment, os.Stdin, os.Stdout, os.Stderr))
	}
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
