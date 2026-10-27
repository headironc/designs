//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/headironc/designs/runner/internal/watchdog"
)

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, `{"event":"watchdog_failed","stage":"arguments"}`)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(watchdog.Run(ctx, os.Stdout))
}
