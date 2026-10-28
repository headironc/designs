package firewall

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func stateFixture(t *testing.T) (string, *firewallFiles) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := openFirewallFiles(path, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return path, s
}

func TestFirewallPermanentLockExcludesConcurrentWriterAndPreservesIdentity(t *testing.T) {
	path, s := stateFixture(t)
	if _, err := openFirewallFiles(path, uint32(os.Getuid())); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("concurrent owner entered: %v", err)
	}
	if err := s.reserve(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "operation.lock")); err != nil {
		t.Fatal(err)
	}
	if s.unchanged() {
		t.Fatal("lost lock path retained acceptance")
	}
	if _, err := openFirewallFiles(path, uint32(os.Getuid())); err == nil {
		t.Fatal("recreated missing lock beside original active slot")
	}
	if _, err := os.Lstat(filepath.Join(path, "operation.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refusal created replacement permanent lock")
	}
	if err := s.create("intent.json", []byte(`{"original":true}`)); err == nil {
		t.Fatal("displaced owner persisted new intent")
	}
}

func TestFirewallMissingLockWithArchiveAlsoRefusesRecreation(t *testing.T) {
	path, s := stateFixture(t)
	if err := os.Mkdir(filepath.Join(path, "completed-0123456789abcdef0123456789abcdef"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "operation.lock")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := openFirewallFiles(path, uint32(os.Getuid())); err == nil {
		t.Fatal("recreated permanent lock beside history")
	}
}

func TestFirewallRecordsAreExclusiveDurableAndPinned(t *testing.T) {
	path, s := stateFixture(t)
	if err := s.reserve(); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"operation":"original"}`)
	if err := s.create("intent.json", raw); err != nil {
		t.Fatal(err)
	}
	if err := s.create("intent.json", []byte(`{}`)); !errors.Is(err, os.ErrExist) {
		t.Fatalf("original intent overwritten: %v", err)
	}
	if err := os.Rename(filepath.Join(path, "active", "intent.json"), filepath.Join(path, "active", "original.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "active", "intent.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if s.unchanged() {
		t.Fatal("same-byte replacement accepted as original inode")
	}
	if _, err := s.read("intent.json"); err == nil {
		t.Fatal("read accepted replaced original")
	}
}

func TestFirewallSyncFailureRetainsIntentAndOriginalSlot(t *testing.T) {
	path, s := stateFixture(t)
	if err := s.reserve(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("original directory sync failed")
	s.sync = func(file *os.File) error {
		if file == s.active {
			return failure
		}
		return file.Sync()
	}
	if err := s.create("intent.json", []byte(`{"retained":true}`)); !errors.Is(err, failure) {
		t.Fatalf("sync failure lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "active", "intent.json")); err != nil {
		t.Fatal("sync failure removed original intent")
	}
	if err := s.reserve(); err == nil {
		t.Fatal("reserved second slot after failed original durability")
	}
}

func TestFirewallArchivePreservesRecordsWithoutOverwritingHistory(t *testing.T) {
	path, s := stateFixture(t)
	if err := s.reserve(); err != nil {
		t.Fatal(err)
	}
	if err := s.create("intent.json", []byte(`{"original":true}`)); err != nil {
		t.Fatal(err)
	}
	operation := "0123456789abcdef0123456789abcdef"
	if err := s.archive(operation); err == nil {
		t.Fatal("archived unresolved original intent")
	}
	if err := s.create("result.json", []byte(`{"known":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.archive(operation); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "completed-"+operation, "intent.json")); err != nil {
		t.Fatal("archive lost original intent")
	}
	if err := s.reserve(); err != nil {
		t.Fatal(err)
	}
	if err := s.create("intent.json", []byte(`{"second":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.create("result.json", []byte(`{"known":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.archive(operation); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote history: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "active", "intent.json")); err != nil {
		t.Fatal("failed archive removed current operation")
	}
}

func TestFirewallStateRejectsSymlinksHardLinksAndInsecureMetadata(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "mode"} {
		t.Run(kind, func(t *testing.T) {
			path, s := stateFixture(t)
			if err := s.reserve(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(path, "active", "intent.json")
			original := filepath.Join(path, "source")
			if err := os.WriteFile(original, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(original, target); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(original, target); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.WriteFile(target, []byte(`{}`), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.read("intent.json"); err == nil {
				t.Fatal("accepted unprotected state record")
			}
		})
	}
}
