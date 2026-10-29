//go:build linux && integration

package firewall

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// Final legacy policy after the independently traced egress/diagnostics ports.
// Keeping the literal in Go also binds every fixture byte to the existing
// reviewed vulnerability input contract without adding untracked embed inputs.
var legacyPolicy = []byte(`# Only Nucleiron traffic is filtered here. No host-wide flush or policy change.
table inet nucleiron {
    chain guest_input {
        type filter hook input priority -20; policy accept;
        iifname "nucleiron0" udp sport 68 udp dport 67 counter accept
        iifname "nucleiron0" ip saddr 10.203.64.0/24 ip daddr 10.203.64.1 udp dport 53 counter accept
        iifname "nucleiron0" ip saddr 10.203.64.0/24 ip daddr 10.203.64.1 tcp dport { 53, 9443, 3128, 6514 } counter accept
        iifname "nucleiron0" counter drop
    }
    chain guest_forward {
        type filter hook forward priority -20; policy accept;
        iifname "nucleiron0" counter drop
        oifname "nucleiron0" counter drop
    }
}
table bridge nucleiron {
    chain guest_ipv6 {
        type filter hook input priority -200; policy accept;
        meta ibrname "nucleiron0" ether type ip6 counter drop
    }
    chain guest_peers {
        type filter hook forward priority -200; policy accept;
        meta ibrname "nucleiron0" counter drop
        meta obrname "nucleiron0" counter drop
    }
}
`)

func namespaceIdentity(t *testing.T, kind string) string {
	t.Helper()
	value, err := os.Readlink("/proc/self/ns/" + kind)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func directParentNamespaces(t *testing.T) (string, string) {
	t.Helper()
	parent := "/proc/" + strconv.Itoa(os.Getppid()) + "/ns/"
	network, err := os.Readlink(parent + "net")
	if err != nil {
		t.Fatal(err)
	}
	user, err := os.Readlink(parent + "user")
	if err != nil {
		t.Fatal(err)
	}
	return network, user
}

func inheritedParentNamespaces(t *testing.T) (string, string) {
	t.Helper()
	identity := func(fd int, kind int) string {
		file := os.NewFile(uintptr(fd), "parent namespace")
		if file == nil {
			t.Fatal("missing inherited parent namespace")
		}
		defer file.Close()
		actual, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
		if err != nil || actual != kind {
			t.Fatalf("invalid inherited namespace type: %v", err)
		}
		name, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
		if err != nil {
			t.Fatal(err)
		}
		return name
	}
	return identity(4, unix.CLONE_NEWNET), identity(3, unix.CLONE_NEWUSER)
}

// This control-only regression never creates a namespace or opens netfilter.
// It verifies the inherited handles before the privileged packet fixture uses
// them as proof, including fail-closed handling of swapped or ordinary files.
func TestNamespaceDescriptorHandoff(t *testing.T) {
	if os.Getenv("RUNNER_FIREWALL_DESCRIPTOR_FIXTURE") == "1" {
		network, user := inheritedParentNamespaces(t)
		if network != namespaceIdentity(t, "net") || user != namespaceIdentity(t, "user") {
			t.Fatal("inherited namespace handles changed identity")
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"valid", "swapped", "ordinary"} {
		t.Run(kind, func(t *testing.T) {
			user, err := os.Open("/proc/self/ns/user")
			if err != nil {
				t.Fatal(err)
			}
			defer user.Close()
			network, err := os.Open("/proc/self/ns/net")
			if err != nil {
				t.Fatal(err)
			}
			defer network.Close()
			files := []*os.File{user, network}
			if kind == "swapped" {
				files[0], files[1] = files[1], files[0]
			}
			if kind == "ordinary" {
				regular, err := os.CreateTemp(t.TempDir(), "not-a-namespace-")
				if err != nil {
					t.Fatal(err)
				}
				defer regular.Close()
				files = []*os.File{regular, regular}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestNamespaceDescriptorHandoff$")
			cmd.Env = append(os.Environ(), "RUNNER_FIREWALL_DESCRIPTOR_FIXTURE=1")
			cmd.ExtraFiles = files
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("namespace descriptor child did not terminate")
			}
			if (err == nil) != (kind == "valid") {
				t.Fatalf("descriptor handoff %s: %v: %s", kind, err, out)
			}
			if err != nil && !bytes.Contains(out, []byte("invalid inherited namespace type")) {
				t.Fatalf("unexpected descriptor refusal: %s", out)
			}
		})
	}
}

func TestReloadInPrivateNamespace(t *testing.T) {
	if os.Getenv("RUNNER_FIREWALL_NAMESPACE") != "1" {
		t.Fatal("enable the documented private namespace test explicitly")
	}
	user, network := namespaceIdentity(t, "user"), namespaceIdentity(t, "net")
	parentNetwork, parentUser := os.Getenv("RUNNER_FIREWALL_PARENT_NAMESPACE"), os.Getenv("RUNNER_FIREWALL_PARENT_USER_NAMESPACE")
	if parentNetwork == "" && parentUser == "" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
		defer cancel()
		parentUserFile, err := os.Open("/proc/self/ns/user")
		if err != nil {
			t.Fatal(err)
		}
		defer parentUserFile.Close()
		parentNetFile, err := os.Open("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		defer parentNetFile.Close()
		cmd := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user", "--net", executable, "-test.run=^TestReloadInPrivateNamespace$", "-test.timeout=45s", "-test.v")
		// Opening the original handles before unshare avoids cross-user-namespace
		// ptrace permissions on /proc/<parent>/ns. The child validates nsfs types.
		cmd.ExtraFiles = []*os.File{parentUserFile, parentNetFile}
		cmd.Env = append(os.Environ(), "RUNNER_FIREWALL_PARENT_NAMESPACE="+network, "RUNNER_FIREWALL_PARENT_USER_NAMESPACE="+user)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("private namespace test: %v: %s", err, out)
		}
		t.Log(string(out))
		return
	}
	if parentUser == "" || parentNetwork == "" || network == parentNetwork || user == parentUser {
		t.Fatal("refusing a network or user namespace without proved isolation")
	}
	if os.Getenv("RUNNER_FIREWALL_ACTOR") != "" {
		packetActor(t)
		return
	}
	actualParentNet, actualParentUser := inheritedParentNamespaces(t)
	if actualParentNet != parentNetwork || actualParentUser != parentUser {
		t.Fatal("namespace markers do not match the live original parent")
	}
	for _, tool := range []string{"ip", "nft", "unshare"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("private fixture requires %s: %v", tool, err)
		}
	}
	if digestBytes(legacyPolicy) != "b09925ba63f7518d1ae49555c5fc0bcd42b8a773f41aac9e1a2e190067ada270" {
		t.Fatal("legacy source fixture changed")
	}

	invoke := func(input []byte, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "nft", args...)
		cmd.Stdin = bytes.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("private nft %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	invoke(nil, "add", "table", "inet", "unrelated")
	invoke(nil, "add", "chain", "inet", "unrelated", "preserve")
	invoke(nil, "add", "table", "ip", "nucleiron")
	unrelated := invoke(nil, "list", "table", "inet", "unrelated")
	otherFamily := invoke(nil, "list", "table", "ip", "nucleiron")
	fixture := setupPacketFixture(t)
	fixture.check(t, "unfiltered fixture", false)

	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	configuration := filepath.Join(root, "firewall.json")
	raw, err := RenderConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configuration, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Reload(context.Background(), configuration, state); err != nil {
		t.Fatalf("first native load: %v", err)
	}
	fixture.check(t, "first native policy", true)
	first := nativePolicyDigest(t)
	if err := os.WriteFile(configuration, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Reload(context.Background(), configuration, state); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	if nativePolicyDigest(t) != first {
		t.Fatal("invalid input changed original rules")
	}
	if err := os.WriteFile(configuration, raw, 0600); err != nil {
		t.Fatal(err)
	}

	// Only this disposable namespace loses its owned rules for the independent
	// legacy comparison. The product never uses these CLI operations.
	invoke(nil, "delete", "table", "inet", "nucleiron")
	invoke(nil, "delete", "table", "bridge", "nucleiron")
	invoke(legacyPolicy, "-f", "-")
	fixture.check(t, "legacy final policy", true)
	if err := Reload(context.Background(), configuration, state); err != nil {
		t.Fatalf("replace legacy sets with native policy: %v", err)
	}
	fixture.check(t, "native replacement", true)
	if nativePolicyDigest(t) == first {
		t.Fatal("explicit reload reused original operation marker")
	}
	assertKernelAtomicRejection(t)
	if invoke(nil, "list", "table", "inet", "unrelated") != unrelated || invoke(nil, "list", "table", "ip", "nucleiron") != otherFamily {
		t.Fatal("a table outside the two owned tables changed")
	}
}

func nativePolicyDigest(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	k, err := openKernel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	policy, err := k.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := policyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func assertKernelAtomicRejection(t *testing.T) {
	t.Helper()
	before := nativePolicyDigest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	k, err := openKernel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	table := &nftables.Table{Name: "nucleiron", Family: nftables.TableFamilyINet}
	k.Conn.DelTable(table)
	k.Conn.AddRule(&nftables.Rule{Table: table, Chain: &nftables.Chain{Table: table, Name: "missing"}, Exprs: []expr.Any{&expr.Counter{}}})
	flushErr := k.Flush()
	closeErr := k.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if flushErr == nil || kernelRejection(flushErr) == 0 {
		t.Fatalf("kernel did not confirm transaction rejection: %v", flushErr)
	}
	if nativePolicyDigest(t) != before {
		t.Fatal("rejected transaction partially deleted previous rules")
	}
}
