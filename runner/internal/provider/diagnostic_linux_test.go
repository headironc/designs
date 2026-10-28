package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticGuardObservedWithoutMutation(t *testing.T) {
	for _, mode := range []string{"active", "clear", "intent", "pending", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			s := executionFixture(t)
			path := filepath.Join(s.state, "state.json")
			if mode == "clear" {
				write(t, path, []byte(clearState), 0600)
			}
			if mode == "intent" {
				raw, _ := os.ReadFile(path)
				write(t, path, []byte(strings.Replace(string(raw), `"intent":null`, `"intent":{"operation":"unknown"}`, 1)), 0600)
			}
			if mode == "pending" {
				write(t, filepath.Join(s.state, "state.pending"), []byte("unknown"), 0600)
			}
			if mode == "maintenance" {
				write(t, s.maintenance, []byte("unknown"), 0600)
			}
			before := fileSHA(t, path)
			got, err := s.observeGuard(context.Background())
			if mode == "pending" || mode == "maintenance" {
				if err == nil {
					t.Fatal("pending guard admitted")
				}
			} else {
				if err != nil || got.PolicySHA != fileSHA(t, s.policy) || got.StateSHA != before || got.ScaleSet != "build" || got.Pool != "build-cccccccc-0000-4000-8000-000000000003" {
					t.Fatalf("%+v %v", got, err)
				}
				if mode == "intent" && !got.Intent {
					t.Fatal("intent hidden")
				}
			}
			if fileSHA(t, path) != before {
				t.Fatal("diagnosis changed state")
			}
		})
	}
}
