package lifecycle

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionWindowAdmissionRetainsRuntimeAndDatabasePrincipal(t *testing.T) {
	w, p, raw, b := sessionWindowFixture(t)
	ready, err := w.execute(context.Background(), p, raw)
	if err != nil {
		t.Fatal(err)
	}
	user := sessionDatabase{Controller: p.Controller, User: "eeeeeeee-0000-4000-8000-000000000005", Generation: 7}
	a, err := w.loginAdmission(p, ready, user)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	a.passwordPath = filepath.Join(w.store.files.boundary, "password")
	testWrite(t, a.passwordPath, []byte("fixture-admin-password\n"), 0600)
	if a.check(context.Background()) != nil || a.origin().User != user.User || a.origin().Generation != 7 {
		t.Fatal("live window lost its admitted principal")
	}
	password, err := a.password()
	if err != nil || password != "fixture-admin-password" {
		t.Fatal("protected password source refused")
	}
	if len(b.sends) != 3 {
		t.Fatal("admission issued another service request")
	}
	b.after.Invocation = strings.Repeat("3", 32)
	if a.check(context.Background()) == nil || len(b.sends) != 3 {
		t.Fatal("a replacement invocation reused the admitted window")
	}
}
