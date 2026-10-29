package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionConsumerRequiresCompletedBoundProducerAndLifetime(t *testing.T) {
	a, request, _, _ := sessionAcquisitionFixture(t)
	if _, err := a.execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.parent, "session", "garm-token")
	for _, producer := range []string{"", strings.Repeat("b", 64)} {
		if token, err := loadGARMToken(a.files, path, a.parent, producer, a.now()); err == nil {
			token.close()
			t.Fatal("unbound producer accepted")
		}
	}
	token, err := loadGARMToken(a.files, path, a.parent, a.producer, a.now())
	if err != nil || token.token != sessionJWT(sessionClaims) || token.controller != "bbbbbbbb-0000-4000-8000-000000000002" {
		t.Fatal("verified session was not consumable")
	}
	second, err := loadGARMToken(a.files, path, a.parent, a.producer, a.now())
	if err != nil {
		t.Fatal("independent consumers could not share a verified credential")
	}
	if unexpected, err := openServiceFiles(a.files, a.parent, "session", "session.lock", false); err == nil {
		unexpected.close()
		t.Fatal("credential mutation acquired an active consumer's lease")
	}
	token.close()
	second.close()
	writer, err := openServiceFiles(a.files, a.parent, "session", "session.lock", false)
	if err != nil {
		t.Fatal("consumer lease survived both consumers")
	}
	writer.close()
	if token, err := loadGARMToken(a.files, path, a.parent, a.producer, time.Unix(1863216000, 0)); err == nil {
		token.close()
		t.Fatal("expired session was consumable")
	}
	seal := filepath.Join(a.parent, "session", "complete.json")
	if os.Rename(seal, seal+".fixture") != nil {
		t.Fatal("fixture seal mutation")
	}
	if token, err := loadGARMToken(a.files, path, a.parent, a.producer, a.now()); err == nil {
		token.close()
		t.Fatal("unsealed raw token was consumable")
	}
}

func TestSessionTokenOriginIsSealedBeforeRawPublication(t *testing.T) {
	a, request, _, _ := sessionAcquisitionFixture(t)
	seen := false
	a.files.syncHook = func(f *os.File) error {
		if filepath.Base(f.Name()) == "garm-token" {
			seen = true
			if _, err := os.Stat(filepath.Join(a.parent, "session", "verified.json")); err != nil {
				t.Fatal("raw token preceded its verified original judgment")
			}
		}
		return f.Sync()
	}
	if _, err := a.execute(context.Background(), request); err != nil || !seen {
		t.Fatal("token publication did not complete")
	}
}

func TestSessionRetirementMarkerPreventsNewConsumers(t *testing.T) {
	a, request, _, _ := sessionAcquisitionFixture(t)
	if _, err := a.execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	testWrite(t, filepath.Join(a.parent, "session", "retired.json"), []byte(`{"state":"retired"}`), 0600)
	if token, err := loadGARMToken(a.files, filepath.Join(a.parent, "session", "garm-token"), a.parent, a.producer, a.now()); err == nil {
		token.close()
		t.Fatal("retired session was consumable")
	}
}

func TestSessionExternalCredentialRetainsExistingContract(t *testing.T) {
	a, _, _, _ := sessionAcquisitionFixture(t)
	path := filepath.Join(a.parent, "external-token")
	testWrite(t, path, []byte("fixture-existing-credential\n"), 0600)
	token, err := loadGARMToken(a.files, path, a.parent, "", a.now())
	if err != nil || token.token != "fixture-existing-credential" || token.controller != "" || token.store != nil {
		t.Fatal("existing external credential contract changed")
	}
	token.close()
}

func TestCaptureCredentialsPrecedeReservationAndOriginalReadbackIsIndependent(t *testing.T) {
	c, path, operations := captureFixture(t)
	c.credentials = func() error { return errRefused }
	if stage, err := c.execute(context.Background(), path, operations); err == nil || stage != "credential" {
		t.Fatal("missing credential did not refuse before observation")
	}
	if entries, err := os.ReadDir(operations); err != nil || len(entries) != 0 {
		t.Fatal("credential refusal reserved an execution")
	}
	c.credentials = func() error { return nil }
	if _, err := c.execute(context.Background(), path, operations); err != nil {
		t.Fatal(err)
	}
	c.credentials = func() error {
		t.Fatal("original readback attempted live credential admission")
		return errRefused
	}
	if stage, err := c.execute(context.Background(), path, operations); err != nil || stage != "existing" {
		t.Fatal("original result depended on live credentials")
	}
}
