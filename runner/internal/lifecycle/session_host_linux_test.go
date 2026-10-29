package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionGatewayAdmissionDuringControllerWindow(t *testing.T) {
	for _, mode := range []string{"controller_running", "controller_stopped", "gateway_running", "gateway_job", "populated", "reverse_stop", "controller_unused"} {
		t.Run(mode, func(t *testing.T) {
			g, fixture, _, _ := gatewayFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			connection, err := g.connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.close()
			base := connection.(gatewayTestConnection).gatewayConnection.(*gatewayBus)
			fixture.mu.Lock()
			fixture.stopped = mode != "gateway_running"
			fixture.controllerGone = mode == "controller_stopped"
			switch mode {
			case "gateway_job":
				fixture.mode = "late_job"
			case "reverse_stop", "controller_unused":
				fixture.mode = mode
			}
			fixture.mu.Unlock()
			if mode != "gateway_running" {
				if err := os.RemoveAll(filepath.Join(fixture.host.proc, "9001")); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "populated" {
				testWrite(t, filepath.Join(fixture.host.cgroup, gatewayGroup, "cgroup.events"), []byte("populated 0\n"), 0600)
			}
			b := &sessionBus{unitBus: base.unitBus, host: base.host}
			sha, err := b.gatewayIdle(ctx)
			allowed := mode == "controller_running" || mode == "controller_stopped"
			if (err == nil) != allowed || allowed && !digestPattern.MatchString(sha) {
				t.Fatal("gateway admission", sha, err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.sends != 0 {
				t.Fatal("credential admission sent a gateway request")
			}
		})
	}
}
