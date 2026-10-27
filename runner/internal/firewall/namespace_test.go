//go:build linux && integration

package firewall

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReloadInPrivateNamespace(t *testing.T) {
	if os.Getenv("RUNNER_FIREWALL_NAMESPACE") != "1" {
		t.Fatal("enable the documented private namespace test explicitly")
	}
	self, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	initial := os.Getenv("RUNNER_FIREWALL_PARENT_NAMESPACE")
	if initial == "" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user", "--net", executable, "-test.run=^TestReloadInPrivateNamespace$", "-test.timeout=20s", "-test.v")
		cmd.Env = append(os.Environ(), "RUNNER_FIREWALL_PARENT_NAMESPACE="+self)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("private namespace test: %v: %s", err, out)
		}
		t.Log(string(out))
		return
	}
	if self == initial {
		t.Fatalf("refusing the parent network namespace: %q", self)
	}
	nft := "/usr/sbin/nft"
	invoke := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(nft, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("nft %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	invoke("add", "table", "inet", "unrelated")
	invoke("add", "chain", "inet", "unrelated", "preserve")
	invoke("add", "table", "ip", "nucleiron")
	unrelated := invoke("list", "table", "inet", "unrelated")
	otherFamily := invoke("list", "table", "ip", "nucleiron")
	rules := filepath.Join(t.TempDir(), "rules.nft")
	first := "table inet nucleiron {\n chain first {}\n}\ntable bridge nucleiron {\n chain first {}\n}\n"
	second := "table inet nucleiron {\n chain second {}\n}\ntable bridge nucleiron {\n chain second {}\n}\n"
	for _, body := range []string{first, second} {
		if err := os.WriteFile(rules, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Reload(context.Background(), nft, rules); err != nil {
			t.Fatal(err)
		}
	}
	for _, family := range []string{"inet", "bridge"} {
		current := invoke("list", "table", family, "nucleiron")
		if !strings.Contains(current, "chain second") || strings.Contains(current, "chain first") {
			t.Fatalf("old owned table was not replaced: %s", current)
		}
	}
	beforeInvalid := invoke("list", "ruleset")
	if err := os.WriteFile(rules, []byte("invalid nft syntax\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Reload(context.Background(), nft, rules); err == nil {
		t.Fatal("invalid rules accepted")
	}
	if after := invoke("list", "ruleset"); after != beforeInvalid {
		t.Fatal("failed check changed the ruleset")
	}
	if invoke("list", "table", "inet", "unrelated") != unrelated || invoke("list", "table", "ip", "nucleiron") != otherFamily {
		t.Fatal("a table outside the two owned tables changed")
	}
}
