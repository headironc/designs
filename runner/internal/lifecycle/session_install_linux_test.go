package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func sessionInstallerFixture(t *testing.T) (sessionInstaller, []byte, []byte) {
	t.Helper()
	root := t.TempDir()
	parent := filepath.Join(root, "credentials")
	if os.Mkdir(parent, 0700) != nil {
		t.Fatal("fixture parent")
	}
	s, err := openServiceFiles(protectedFiles{uid: uint32(os.Getuid()), boundary: root}, parent, "window", "session.lock", true)
	if err != nil || s.reserve() != nil {
		t.Fatal("fixture window")
	}
	t.Cleanup(s.close)
	before, entropy := sessionConfigurationFixture(t)
	target := filepath.Join(root, "etc", "garm.toml")
	testWrite(t, target, before, 0640)
	i := sessionInstaller{store: s, path: target, gid: uint32(os.Getgid()), stopped: func(context.Context) error { return nil }}
	return i, before, entropy
}

func TestSessionConfigurationInstallationAndOriginalRecovery(t *testing.T) {
	i, before, entropy := sessionInstallerFixture(t)
	if err := i.prepare(bytesSHA(before), bytes.NewReader(entropy)); err != nil {
		t.Fatal(err)
	}
	// Recovery must consume the original candidate, never draw a second key.
	if err := i.prepare(bytesSHA(before), nil); err != nil {
		t.Fatal("original candidate was not recoverable")
	}
	if err := i.apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, candidate, err := i.original()
	if err != nil {
		t.Fatal(err)
	}
	current, stat, err := i.read(i.path, 0640)
	if err != nil || !bytes.Equal(current, candidate) || bytes.Equal(current, before) {
		t.Fatal("candidate did not replace original configuration")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(i.path), ".garm-session-new")); !os.IsNotExist(err) {
		t.Fatal("old key staging copy was not cleaned up")
	}
	i.stopped = func(context.Context) error {
		t.Fatal("original installation readback attempted another service action")
		return errRefused
	}
	if err := i.apply(context.Background()); err != nil {
		t.Fatal("original configuration transaction could not be read back")
	}
	again, afterStat, err := i.read(i.path, 0640)
	if err != nil || !bytes.Equal(again, candidate) || stat.Ino != afterStat.Ino {
		t.Fatal("original recovery replaced the installed configuration")
	}
}

func TestSessionInstallationRequiresProvenOriginalStop(t *testing.T) {
	i, before, entropy := sessionInstallerFixture(t)
	if err := i.prepare(bytesSHA(before), bytes.NewReader(entropy)); err != nil {
		t.Fatal(err)
	}
	i.stopped = func(context.Context) error { return errRefused }
	if err := i.apply(context.Background()); err == nil {
		t.Fatal("unproven stop allowed configuration replacement")
	}
	actual, _, err := i.read(i.path, 0640)
	if err != nil || !bytes.Equal(actual, before) {
		t.Fatal("refused installation changed the original")
	}
}

func TestSessionInstallationRecoversExchangeWithoutReplacingAgain(t *testing.T) {
	i, before, entropy := sessionInstallerFixture(t)
	if err := i.prepare(bytesSHA(before), bytes.NewReader(entropy)); err != nil {
		t.Fatal(err)
	}
	_, _, candidate, err := i.original()
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	i.store.files.syncHook = func(f *os.File) error {
		if f.Name() == filepath.Dir(i.path) && !failed {
			current, _ := os.ReadFile(i.path)
			if bytes.Equal(current, candidate) {
				failed = true
				return errors.New("fixture directory synchronization unknown")
			}
		}
		return f.Sync()
	}
	if err := i.apply(context.Background()); err == nil || !failed {
		t.Fatal("exchange interruption was not retained")
	}
	_, first, err := i.read(i.path, 0640)
	if err != nil {
		t.Fatal(err)
	}
	i.store.files.syncHook = nil
	if err := i.apply(context.Background()); err != nil {
		t.Fatal("original exchange could not finish its receipt")
	}
	current, second, err := i.read(i.path, 0640)
	if err != nil || first.Ino != second.Ino || !bytes.Equal(current, candidate) {
		t.Fatal("recovery performed another replacement")
	}
}

func TestSessionInstallationSealSyncFailureRecoversOriginal(t *testing.T) {
	i, before, entropy := sessionInstallerFixture(t)
	if err := i.prepare(bytesSHA(before), bytes.NewReader(entropy)); err != nil {
		t.Fatal(err)
	}
	i.store.files.syncHook = func(f *os.File) error {
		if filepath.Base(f.Name()) == "configuration.installed.json" {
			return errors.New("fixture installation seal sync unknown")
		}
		return f.Sync()
	}
	if err := i.apply(context.Background()); err == nil {
		t.Fatal("unknown installation seal was accepted")
	}
	i.store.files.syncHook = nil
	if err := i.apply(context.Background()); err != nil {
		t.Fatal("original seal did not recover")
	}
}

func TestSessionInstallationRefusesOldKeyRestoration(t *testing.T) {
	i, before, entropy := sessionInstallerFixture(t)
	if err := i.prepare(bytesSHA(before), bytes.NewReader(entropy)); err != nil {
		t.Fatal(err)
	}
	if err := i.apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	testWrite(t, i.path, before, 0640)
	if err := i.apply(context.Background()); err == nil {
		t.Fatal("old authentication domain was accepted after transition")
	}
}

func TestSessionInstallationPreservesForeignStagingObject(t *testing.T) {
	i, before, entropy := sessionInstallerFixture(t)
	if err := i.prepare(bytesSHA(before), bytes.NewReader(entropy)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(i.path), ".garm-session-new")
	testWrite(t, path, []byte("unrelated local work"), 0600)
	var original unix.Stat_t
	if unix.Stat(path, &original) != nil {
		t.Fatal("fixture metadata")
	}
	if err := i.apply(context.Background()); err == nil {
		t.Fatal("foreign staging object was adopted")
	}
	data, err := os.ReadFile(path)
	var after unix.Stat_t
	if err != nil || string(data) != "unrelated local work" || unix.Stat(path, &after) != nil || original.Ino != after.Ino {
		t.Fatal("foreign staging object changed")
	}
}
