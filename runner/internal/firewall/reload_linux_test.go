package firewall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedConfigurationRejectsReplacementAndPolicyWidening(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "firewall.json")
	if err := os.WriteFile(path, []byte(finalConfiguration), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := openConfiguration(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.unchanged() {
		t.Fatal("original config not accepted")
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(finalConfiguration), 0600); err != nil {
		t.Fatal(err)
	}
	if c.unchanged() {
		t.Fatal("same-byte config replacement accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"bridge":"any"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if next, err := openConfiguration(path, uint32(os.Getuid())); err == nil {
		next.Close()
		t.Fatal("widened config loaded")
	}
}

func TestProtectedConfigurationRejectsWritableParentAndRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "firewall.json")
	if err := os.WriteFile(path, []byte(finalConfiguration), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	if next, err := openConfiguration(path, uint32(os.Getuid())); err == nil {
		next.Close()
		t.Fatal("writable parent accepted")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if next, err := openConfiguration(path, uint32(os.Getuid())); err == nil {
		next.Close()
		t.Fatal("unprotected config mode accepted")
	}
}

func TestCanceledReloadDoesNotOpenRuntimeInputsOrKernel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Reload(ctx, "/nonexistent-configuration", "/nonexistent-state"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reload: %v", err)
	}
}
