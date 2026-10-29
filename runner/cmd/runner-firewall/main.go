// runner-firewall reloads only the dedicated Nucleiron nftables tables.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/headironc/designs/runner/internal/firewall"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "render-config" {
		raw, err := firewall.RenderConfiguration()
		if err == nil {
			_, err = os.Stdout.Write(raw)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "render firewall configuration:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "runner-firewall accepts only render-config or no arguments")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := firewall.Reload(ctx, "/etc/nucleiron-gateway/firewall.json", "/var/lib/nucleiron-firewall"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
