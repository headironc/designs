package lifecycle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Independent projection of GARM v0.2.1 database/sql/models.go: Base.ID is
// uuid; relation UUIDs are text; ScaleSetFKID is integer; endpoint name has
// explicit varchar(64). None of these fields gains an invented NOT NULL.
const fixtureDDL = `
CREATE TABLE controller_infos(id uuid PRIMARY KEY,controller_id text,deleted_at datetime);
CREATE TABLE repositories(id uuid PRIMARY KEY,owner text,name text,endpoint_name text,deleted_at datetime);
CREATE TABLE github_endpoints(name varchar(64) PRIMARY KEY,endpoint_type text,api_base_url text,base_url text,deleted_at datetime);
CREATE TABLE scale_sets(id integer PRIMARY KEY,scale_set_id integer,repo_id text,name text,provider_name text,enabled numeric,org_id text,enterprise_id text,deleted_at datetime);
CREATE TABLE pools(id uuid PRIMARY KEY,enabled numeric,deleted_at datetime);
CREATE TABLE instances(id uuid PRIMARY KEY,name text,provider_id text,agent_id integer,scale_set_fk_id integer,pool_id text,status text,runner_status text,deleted_at datetime);
`

func databaseFixture(t *testing.T, dir string) (*sql.DB, databaseFiles, receipt) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "garm.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, q := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`, fixtureDDL} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := originalFixture(t)
	for _, q := range []string{
		`INSERT INTO controller_infos VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','bbbbbbbb-0000-4000-8000-000000000002',NULL)`,
		`INSERT INTO repositories VALUES('cccccccc-0000-4000-8000-000000000003','example','consumer','public',NULL)`,
		`INSERT INTO github_endpoints VALUES('public','github','https://api.github.com/','https://github.com',NULL)`,
		`INSERT INTO scale_sets VALUES(27,99,'cccccccc-0000-4000-8000-000000000003','build','halo-lxd',0,NULL,NULL,NULL)`,
		`INSERT INTO instances VALUES('dddddddd-0000-4000-8000-000000000004','runner-one','runner-one',9007199254740999,27,NULL,'running','active',NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// SQLite inherits ordinary DB permissions for WAL/SHM. Assert, don't repair.
	files := databaseFiles{path: path, boundary: dir, uid: uint32(os.Geteuid()), gid: uint32(os.Getegid())}
	return db, files, r
}

func TestDatabaseWALOnlyAndReadOnlySnapshot(t *testing.T) {
	db, files, r := databaseFixture(t, "")
	wal, err := os.ReadFile(files.path + "-wal")
	if err != nil || len(wal) <= 32 {
		t.Fatal("no committed WAL fixture")
	}
	main, _ := os.ReadFile(files.path)
	if bytes.Contains(main, []byte("runner-one")) {
		t.Fatal("fixture row was checkpointed into main DB")
	}
	got := readDatabase(context.Background(), files, r)
	if got.State != "observed" || got.Target != "present" || got.TargetStatus != "running" || got.Count != 1 || got.EnabledScaleSets != 0 {
		t.Fatalf("WAL row lost: %+v", got)
	}
	afterMain, _ := os.ReadFile(files.path)
	afterWAL, _ := os.ReadFile(files.path + "-wal")
	if !bytes.Equal(main, afterMain) || !bytes.Equal(wal, afterWAL) {
		t.Fatal("read diagnostic wrote database/WAL")
	}
	ro, err := sql.Open("sqlite", "file:"+files.path+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	tx, err := ro.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := databaseSchema(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE instances SET status='stopped'`); err != nil {
		t.Fatal(err)
	}
	old, err := databaseRows(context.Background(), tx, r)
	if err != nil || old.TargetStatus != "running" {
		t.Fatalf("single transaction changed snapshot: %+v %v", old, err)
	}
	if _, err := tx.Exec(`UPDATE instances SET status='error'`); err == nil {
		t.Fatal("query_only permitted SQL write")
	}
	tx.Rollback()
	if got := readDatabase(context.Background(), files, r); got.TargetStatus != "stopped" {
		t.Fatalf("new transaction did not see committed WAL: %+v", got)
	}
}

func TestDatabaseSchemaTypesAndResiduals(t *testing.T) {
	for _, tc := range []struct{ q, state, target string }{
		{`UPDATE scale_sets SET enabled=NULL`, "unknown", ""},
		{`UPDATE scale_sets SET enabled=2`, "unknown", ""},
		{`UPDATE scale_sets SET enabled='false'`, "unknown", ""},
		{`UPDATE scale_sets SET enabled=1`, "observed", "present"},
		{`UPDATE scale_sets SET org_id='other'`, "unknown", ""},
		{`UPDATE instances SET agent_id=1`, "mismatch", "mismatch"},
		{`UPDATE instances SET scale_set_fk_id=99`, "mismatch", "mismatch"},
		{`UPDATE instances SET deleted_at='2026-10-28'`, "residual", "soft_deleted"},
		{`DELETE FROM instances`, "observed", "absent_verified"},
		{`INSERT INTO instances VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','other','other',2,27,NULL,'running','active',NULL)`, "residual", "present"},
		{`INSERT INTO pools VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',NULL,NULL)`, "unknown", ""},
		{`ALTER TABLE github_endpoints RENAME TO old_endpoint; CREATE TABLE github_endpoints(name text PRIMARY KEY,endpoint_type text,api_base_url text,base_url text,deleted_at datetime)`, "unknown", ""},
		{`ALTER TABLE instances RENAME COLUMN scale_set_fk_id TO scale_set_id`, "unknown", ""},
	} {
		t.Run(tc.q, func(t *testing.T) {
			db, files, r := databaseFixture(t, "")
			if _, err := db.Exec(tc.q); err != nil {
				t.Fatal(err)
			}
			got := readDatabase(context.Background(), files, r)
			if got.State != tc.state || got.Target != tc.target {
				t.Fatalf("%+v", got)
			}
		})
	}
	for _, reverse := range []bool{false, true} {
		db, files, r := databaseFixture(t, "")
		if reverse {
			db.Exec(`DELETE FROM instances`)
		}
		if _, err := db.Exec(`INSERT INTO instances VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','other','other',2,27,NULL,'running','active',NULL)`); err != nil {
			t.Fatal(err)
		}
		if reverse {
			db.Exec(`INSERT INTO instances VALUES('dddddddd-0000-4000-8000-000000000004','runner-one','runner-one',1,27,NULL,'running','active',NULL)`)
		} else {
			db.Exec(`UPDATE instances SET agent_id=1 WHERE name='runner-one'`)
		}
		got := readDatabase(context.Background(), files, r)
		if got.Target != "mismatch" || got.AdditionalInstances != 1 {
			t.Fatalf("row order hid mismatch: %+v", got)
		}
	}
}

func TestDatabaseObjectRefusalsAndCancellation(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "mode", "parent", "wal_link", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			_, files, r := databaseFixture(t, "")
			switch mode {
			case "symlink":
				os.Rename(files.path, files.path+"-original")
				os.Symlink(files.path+"-original", files.path)
			case "hardlink":
				os.Link(files.path, files.path+"-link")
			case "mode":
				os.Chmod(files.path, 0644)
			case "parent":
				os.Chmod(filepath.Dir(files.path), 0777)
			case "wal_link":
				os.Link(files.path+"-wal", files.path+"-wal-link")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			if got := readDatabase(ctx, files, r); got.State != "unknown" {
				t.Fatalf("unsafe DB accepted: %+v", got)
			}
		})
	}
}

func TestDatabaseProtectedConfiguration(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := protectedFiles{uid: uint32(os.Geteuid()), boundary: root}
	path := filepath.Join(root, "garm.toml")
	good := "[database]\nbackend='sqlite3'\n[database.sqlite3]\ndb_file='/var/lib/nucleiron/garm.db'\n[unrelated]\npassword='fixture-only'\n"
	for _, body := range []string{good, strings.Replace(good, "sqlite3'", "mysql'", 1), strings.Replace(good, "garm.db", "copy.db", 1), good + "[database]\nbackend='sqlite3'"} {
		testWrite(t, path, []byte(body), 0640)
		config, _, err := pinDatabaseConfiguration(p, path, uint32(os.Getegid()))
		if config != nil {
			config.Close()
		}
		if (body == good) != (err == nil) {
			t.Fatalf("configuration contract: %v", err)
		}
	}
	testWrite(t, path, []byte(good), 0600)
	config, _, err := pinDatabaseConfiguration(p, path, uint32(os.Getegid()))
	if config != nil {
		config.Close()
	}
	if err == nil {
		t.Fatal("wrong config mode accepted")
	}
}

func TestDatabaseChildHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--fixture-child" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "worker":
		os.Exit(databaseWorker(context.Background(), nil, os.Stdin, os.Stdout))
	case "identity":
		groups, _ := os.Getgroups()
		sha, err := runningDigest()
		if err != nil {
			os.Exit(2)
		}
		raw, _ := io.ReadAll(os.Stdin)
		var r receipt
		if json.Unmarshal(raw, &r) != nil {
			os.Exit(2)
		}
		json.NewEncoder(os.Stdout).Encode(databaseObservation{UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), Groups: append([]int{}, groups...), ObserverSHA: sha})
		os.Exit(0)
	case "oversize":
		io.WriteString(os.Stdout, strings.Repeat("x", 40000))
		os.Exit(0)
	case "wait":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(2)
}

func TestDatabaseChildInPrivateContainer(t *testing.T) {
	if os.Getenv("RUNNER_DIAGNOSTIC_PRIVATE_CONTAINER") != "1" {
		t.Skip("requires disposable private container")
	}
	if os.Getuid() != 0 {
		t.Fatal("container fixture requires root")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		t.Fatal("requires disposable Docker filesystem")
	}
	if _, err := os.Stat(filepath.Dir(databasePath)); !os.IsNotExist(err) {
		t.Fatal("private database directory must not preexist")
	}
	exe, err := os.Executable()
	if err != nil || exe != "/tmp/lifecycle.test" {
		t.Fatal("private executable fixture path required")
	}
	uid, gid, err := serviceIdentity()
	if err != nil || uid != 24567 || gid != 24568 {
		t.Fatal("incorrect private NSS fixture")
	}
	r := originalFixture(t)
	args := func(mode string) []string {
		return []string{"-test.run=^TestDatabaseChildHelper$", "--", "--fixture-child", mode}
	}
	expected, err := runningDigest()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate deployment pathname replacement while this original image is
	// running. A pathname-based re-exec would now execute /bin/false.
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.ReadFile("/bin/false")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, replacement, 0755); err != nil {
		t.Fatal(err)
	}
	if link, err := os.Readlink("/proc/self/exe"); err != nil || !strings.HasSuffix(link, " (deleted)") {
		t.Fatalf("original executable was not unlinked: %q %v", link, err)
	}
	got, err := runDatabaseChild(context.Background(), "/proc/self/exe", args("identity"), r, uid, gid)
	if err != nil || got.ObserverSHA != expected || got.UID != 24567 || got.GID != 24568 || len(got.Groups) != 0 {
		t.Fatalf("child image/identity: %+v %v", got, err)
	}
	t.Logf("same image %s UID=%d GID=%d groups=%v", got.ObserverSHA, got.UID, got.GID, got.Groups)
	db, files, _ := databaseFixture(t, filepath.Dir(databasePath))
	// Fixture setup is confined to the container's new filesystem. The actual
	// observer must not repair ownership or execute any SQL as root.
	for _, path := range []string{filepath.Dir(databasePath), files.path, files.path + "-wal", files.path + "-shm"} {
		if err := os.Chown(path, int(uid), int(gid)); err != nil {
			t.Fatal(err)
		}
	}
	got, err = runDatabaseChild(context.Background(), "/proc/self/exe", args("worker"), r, uid, gid)
	if err != nil || got.Target != "present" || got.State != "observed" || got.ObserverSHA != expected {
		t.Fatalf("service WAL observer: %+v %v", got, err)
	}
	if _, err := runDatabaseChild(context.Background(), "/proc/self/exe", args("worker"), r, uid+1, gid); err == nil {
		t.Fatal("wrong child identity accepted")
	}
	if _, err := runDatabaseChild(context.Background(), "/proc/self/exe", args("worker"), r, uid, gid+1); err == nil {
		t.Fatal("wrong primary group accepted")
	}
	for _, mode := range []string{"oversize", "wait"} {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := runDatabaseChild(ctx, "/proc/self/exe", args(mode), r, uid, gid)
		cancel()
		if err == nil {
			t.Fatalf("child %s accepted", mode)
		}
	}
	// Wrong owner and hard links are observed as failures, never fixed.
	if err := os.Chown(files.path, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, err = runDatabaseChild(context.Background(), "/proc/self/exe", args("worker"), r, uid, gid)
	if err != nil || got.State != "unknown" || got.Code != "database_files" {
		t.Fatalf("owner defect not observed: %+v %v", got, err)
	}
	db.Close()
	t.Log("private service UID/GID and rejection tests complete: " + strconv.Itoa(int(uid)))
}
