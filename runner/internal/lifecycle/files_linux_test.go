package lifecycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/headironc/designs/runner/internal/provider"
	"golang.org/x/sys/unix"
)

func TestProtectedInputFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "parent symlink", "hardlink", "mode", "fifo", "directory", "oversize", "writable parent"} {
		t.Run(kind, func(t *testing.T) {
			c, path, _ := captureFixture(t)
			switch kind {
			case "symlink":
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-original", path); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				real := filepath.Join(filepath.Dir(path), "real")
				testWrite(t, filepath.Join(real, "file"), []byte(requestJSON), 0600)
				alias := filepath.Join(filepath.Dir(path), "alias")
				if err := os.Symlink(real, alias); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(alias, "file")
			case "hardlink":
				if err := os.Link(path, path+"-link"); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				testWrite(t, path, []byte(strings.Repeat("x", 64*1024+1)), 0600)
			case "writable parent":
				if err := os.Chmod(filepath.Dir(path), 0777); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.files.read(path, 0600, 64*1024); err == nil {
				t.Fatal("unprotected input accepted")
			}
		})
	}
}

func TestCLIAdmissionAndRedaction(t *testing.T) {
	for _, args := range [][]string{nil, {"--unknown=fixture-sensitive-value"}, {"capture-execution", "--api-url=https://fixture-sensitive-value"}, {"capture-execution", "--request=fixture-sensitive-value"}} {
		var output, diagnostic bytes.Buffer
		if Run(context.Background(), args, &output, &diagnostic) == 0 || output.Len() != 0 || diagnostic.String() != "{\"event\":\"execution_capture_refused\",\"stage\":\"arguments\"}\n" {
			t.Fatal("arguments escaped fixed diagnostic")
		}
	}
	if os.Getuid() != 0 {
		var output, diagnostic bytes.Buffer
		if Run(context.Background(), []string{"capture-execution", "--request=/absent", "--operations=/absent", "--github-token-file=/absent", "--garm-token-file=/absent"}, &output, &diagnostic) == 0 || !strings.Contains(diagnostic.String(), `"stage":"identity"`) {
			t.Fatal("non-root identity was accepted")
		}
	}
	for _, env := range []string{"GH_TOKEN=x", "GITHUB_API_URL=x", "GARM_CONFIG=x", "LXD_DIR=x", "http_proxy=x", "ALL_PROXY=x", "SSL_CERT_FILE=x", "SSL_CERT_DIR=x", "DEBUG=1", "SWAGGER_DEBUG=1", "GODEBUG=http2debug=2", "GODEBUG=x=1,http2debug=1"} {
		if controlledEnvironment([]string{env}) {
			t.Fatalf("uncontrolled environment accepted: %s", strings.Split(env, "=")[0])
		}
	}
	if !controlledEnvironment([]string{"PATH=/usr/bin", "HOME=/root", "GODEBUG=randseednop=0", "GH_TOKEN="}) {
		t.Fatal("safe environment refused")
	}
}

func TestOriginalResultConfirmsUncertainSealDurability(t *testing.T) {
	for _, kind := range []string{"seal sync", "directory sync"} {
		t.Run(kind, func(t *testing.T) {
			c, path, operations := captureFixture(t)
			operationID := "aaaaaaaa-0000-4000-8000-000000000001"
			dir := filepath.Join(operations, operationID)
			failed := false
			failRecovery := false
			c.files.syncHook = func(f *os.File) error {
				if failRecovery {
					return errRefused
				}
				if !failed {
					match := kind == "seal sync" && filepath.Base(f.Name()) == "complete.json"
					if kind == "directory sync" && filepath.Base(f.Name()) == operationID {
						_, err := os.Stat(filepath.Join(dir, "complete.json"))
						match = err == nil
					}
					if match {
						failed = true
						return errRefused
					}
				}
				return f.Sync()
			}
			if _, err := c.execute(context.Background(), path, operations); err == nil || !failed {
				t.Fatal("injected sync failure was reported successful")
			}
			original := map[string][]byte{}
			for _, name := range []string{"request.json", "execution.json", "complete.json"} {
				raw, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				original[name] = raw
			}
			c.guard = func(context.Context, func(provider.ExecutionBinding, func() error) (func() error, error)) error {
				t.Fatal("recovery tried a fresh observation")
				return errRefused
			}
			failRecovery = true
			if _, err := c.execute(context.Background(), path, operations); err == nil {
				t.Fatal("unsynchronized original accepted")
			}
			failRecovery = false
			if stage, err := c.execute(context.Background(), path, operations); err != nil || stage != "existing" {
				t.Fatalf("original durability recovery: %s %v", stage, err)
			}
			for name, raw := range original {
				after, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatal("durability recovery rewrote original")
				}
			}
		})
	}
}

func TestExistingCredentialProducerShape(t *testing.T) {
	c, path, _ := captureFixture(t)
	rawToken := "fixture-garm-session-token\n"
	testWrite(t, path, []byte(rawToken), 0600)
	raw, err := c.files.read(path, 0600, 64*1024+1)
	if err != nil || string(raw) != rawToken || !validToken(strings.TrimSpace(string(raw))) {
		t.Fatal("existing root0600/raw-token producer contract rejected")
	}
	if validToken("fixture\nsecret") || validToken(strings.Repeat("x", 64*1024+1)) {
		t.Fatal("malformed/oversize credential accepted")
	}
}
