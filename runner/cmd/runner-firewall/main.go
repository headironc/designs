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
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "runner-firewall does not accept arguments")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := firewall.Reload(ctx, "/usr/sbin/nft", "/etc/nucleiron-gateway/isolated-network.nft"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
