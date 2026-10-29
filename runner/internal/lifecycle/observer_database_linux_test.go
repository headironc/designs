package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/util"
)

// Independently projected from GARM v0.2.1 GithubCredentials, gorm.Model and
// GithubEndpoint. The observer never selects a password or unseals in SQLite.
const observerCredentialsDDL = `CREATE TABLE github_credentials(id integer PRIMARY KEY,name varchar(64),user_id uuid,auth_type text,payload longblob,endpoint_name text,updated_at datetime,deleted_at datetime);`

func observerDatabaseFixtureRows(t *testing.T, db *sql.DB, sp sessionPlan) observerPlan {
	t.Helper()
	pass := make([]byte, 32)
	if _, err := rand.Read(pass); err != nil {
		t.Fatal(err)
	}
	payload, err := util.Seal([]byte(`{"fixture":"encrypted payload, never plaintext key"}`), pass)
	if err != nil {
		t.Fatal(err)
	}
	clear(pass)
	if _, err := db.Exec(observerCredentialsDDL); err != nil {
		t.Fatal(err)
	}
	p := observerPlan{Version: 1, Operation: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", Controller: sp.Controller, SessionProducer: strings.Repeat("1", 64), SessionRequest: strings.Repeat("2", 64), Credential: 1, CredentialName: "nucleiron", Endpoint: "public", Updated: "2026-10-29 00:00:00+00:00", PayloadSHA: bytesSHA(payload), App: 42, Installation: 43, Account: 44, Fingerprint: strings.Repeat("3", 64), RequiredUntil: 1863131400}
	if _, err := db.Exec("INSERT INTO github_credentials VALUES(1,'nucleiron',NULL,'app',?,'public',?,NULL)", payload, p.Updated); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestObserverDatabaseWALOnlyAndStrictSource(t *testing.T) {
	db, files, sp := sessionDatabaseFixture(t)
	p := observerDatabaseFixtureRows(t, db, sp)
	main, _ := os.ReadFile(files.path)
	wal, _ := os.ReadFile(files.path + "-wal")
	out, err := readObserverDatabase(context.Background(), files, p, sp)
	if err != nil || out.Session.Controller != p.Controller || bytesSHA(out.Payload) != p.PayloadSHA || !digestPattern.MatchString(out.Identity) {
		t.Fatal("source snapshot refused", err)
	}
	raw, _ := json.Marshal(out)
	if bytes.Contains(raw, []byte("fixture-private-password-hash")) || bytes.Contains(raw, []byte("encrypted payload, never plaintext key")) {
		t.Fatal("database observer exposed plaintext")
	}
	after, _ := os.ReadFile(files.path)
	afterWAL, _ := os.ReadFile(files.path + "-wal")
	if !bytes.Equal(main, after) || !bytes.Equal(wal, afterWAL) {
		t.Fatal("observer wrote DB/WAL")
	}
	for name, sql := range map[string]string{
		"credential_name":    "UPDATE github_credentials SET name='another'",
		"credential_owner":   "UPDATE github_credentials SET user_id='ffffffff-0000-4000-8000-000000000006'",
		"credential_type":    "UPDATE github_credentials SET auth_type='pat'",
		"credential_payload": "UPDATE github_credentials SET payload=x'00'",
		"credential_update":  "UPDATE github_credentials SET updated_at='2026-10-30'",
		"credential_deleted": "UPDATE github_credentials SET deleted_at='2026-10-30'",
		"endpoint":           "UPDATE github_endpoints SET api_base_url='https://other.example'",
		"view":               "ALTER TABLE github_credentials RENAME TO saved;CREATE VIEW github_credentials AS SELECT * FROM saved",
	} {
		t.Run(name, func(t *testing.T) {
			db, files, sp := sessionDatabaseFixture(t)
			p := observerDatabaseFixtureRows(t, db, sp)
			if _, err := db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			if out, err := readObserverDatabase(context.Background(), files, p, sp); err == nil || len(out.Payload) != 0 {
				t.Fatal("invalid source exposed payload")
			}
		})
	}
}
