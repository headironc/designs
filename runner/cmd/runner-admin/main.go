//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/headironc/designs/runner/internal/lifecycle"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	os.Exit(lifecycle.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
