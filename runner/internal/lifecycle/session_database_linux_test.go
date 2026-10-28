package lifecycle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Independently projected from GARM v0.2.1's User model. Password is present
// to detect accidental exposure, but is not selected by the observer.
const sessionUsersDDL = `CREATE TABLE users(id uuid PRIMARY KEY,username text,generation integer,is_admin numeric,enabled numeric,deleted_at datetime,password varchar(60));
INSERT INTO users VALUES('eeeeeeee-0000-4000-8000-000000000005','haloadmin',7,1,1,NULL,'fixture-private-password-hash');`

func sessionDatabaseFixture(t *testing.T) (*sql.DB, databaseFiles, sessionPlan) {
	t.Helper()
	db, files, r := databaseFixture(t, "")
	if _, err := db.Exec("DELETE FROM instances;" + sessionUsersDDL); err != nil {
		t.Fatal(err)
	}
	return db, files, sessionPlan{Controller: r.Expected.Controller, Username: "haloadmin"}
}

func TestSessionDatabaseWALReadOnlyAndNoExecutionReceipt(t *testing.T) {
	_, files, p := sessionDatabaseFixture(t)
	main, err := os.ReadFile(files.path)
	if err != nil || bytes.Contains(main, []byte("haloadmin")) {
		t.Fatal("user fixture must be committed only to WAL")
	}
	wal, err := os.ReadFile(files.path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	got, err := readSessionDatabase(context.Background(), files, p)
	if err != nil || got.Controller != p.Controller || got.User != "eeeeeeee-0000-4000-8000-000000000005" || got.Generation != 7 || !digestPattern.MatchString(got.Identity) {
		t.Fatalf("pre-job snapshot: %+v %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "password") || strings.Contains(string(raw), "fixture-private") {
		t.Fatal("observer leaked a secret")
	}
	after, _ := os.ReadFile(files.path)
	afterWAL, _ := os.ReadFile(files.path + "-wal")
	if !bytes.Equal(main, after) || !bytes.Equal(wal, afterWAL) {
		t.Fatal("session admission wrote SQLite")
	}
}

func TestSessionDatabaseRejectsIncompleteAdmission(t *testing.T) {
	for name, statement := range map[string]string{
		"missing_user":          "DELETE FROM users",
		"another_user":          "INSERT INTO users SELECT 'ffffffff-0000-4000-8000-000000000006','other',0,0,0,NULL,NULL FROM users",
		"wrong_user":            "UPDATE users SET username='other'",
		"not_admin":             "UPDATE users SET is_admin=0",
		"disabled":              "UPDATE users SET enabled=0",
		"null_admin":            "UPDATE users SET is_admin=NULL",
		"null_enabled":          "UPDATE users SET enabled=NULL",
		"null_generation":       "UPDATE users SET generation=NULL",
		"negative_generation":   "UPDATE users SET generation=-1",
		"deleted_user":          "UPDATE users SET deleted_at='2026-01-01'",
		"wrong_controller":      "UPDATE controller_infos SET controller_id='ffffffff-0000-4000-8000-000000000006'",
		"missing_controller":    "DELETE FROM controller_infos",
		"enabled_scale":         "UPDATE scale_sets SET enabled=1",
		"null_scale":            "UPDATE scale_sets SET enabled=NULL",
		"deleted_enabled_scale": "UPDATE scale_sets SET enabled=1,deleted_at='2026-01-01'",
		"enabled_pool":          "INSERT INTO pools VALUES('ffffffff-0000-4000-8000-000000000006',1,NULL)",
		"instance":              "INSERT INTO instances(id,deleted_at) VALUES('ffffffff-0000-4000-8000-000000000006',NULL)",
		"instance_tombstone":    "INSERT INTO instances(id,deleted_at) VALUES('ffffffff-0000-4000-8000-000000000006','2026-01-01')",
		"users_view":            "ALTER TABLE users RENAME TO saved_users; CREATE VIEW users AS SELECT * FROM saved_users",
		"missing_generation":    "ALTER TABLE users DROP COLUMN generation",
	} {
		t.Run(name, func(t *testing.T) {
			db, files, p := sessionDatabaseFixture(t)
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if result, err := readSessionDatabase(context.Background(), files, p); err == nil || !reflect.DeepEqual(result, sessionDatabase{}) {
				t.Fatalf("unsafe admission accepted: %+v %v", result, err)
			}
		})
	}
}

func TestSessionDatabaseIdentityChangesWithUserGenerationAndAdmissionSet(t *testing.T) {
	db, files, p := sessionDatabaseFixture(t)
	first, err := readSessionDatabase(context.Background(), files, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE users SET generation=8"); err != nil {
		t.Fatal(err)
	}
	second, err := readSessionDatabase(context.Background(), files, p)
	if err != nil || second.Generation != 8 || second.Identity == first.Identity {
		t.Fatal("user authentication state changed without a new identity")
	}
	if _, err := db.Exec("INSERT INTO pools VALUES('ffffffff-0000-4000-8000-000000000006',0,NULL)"); err != nil {
		t.Fatal(err)
	}
	third, err := readSessionDatabase(context.Background(), files, p)
	if err != nil || third.Identity == second.Identity {
		t.Fatal("admission inventory changed without a new identity")
	}
}
