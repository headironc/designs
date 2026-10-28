package lifecycle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const databasePath = "/var/lib/nucleiron/garm.db"
const controllerConfig = "/etc/nucleus-halo/garm.toml"

type databaseObservation struct {
	resourceObservation
	UID                 uint32 `json:"uid"`
	GID                 uint32 `json:"gid"`
	Groups              []int  `json:"groups"`
	EnabledScaleSets    int    `json:"enabled_scalesets"`
	EnabledPools        int    `json:"enabled_pools"`
	Target              string `json:"target"`
	TargetStatus        string `json:"target_status,omitempty"`
	AdditionalInstances int    `json:"additional_instances"`
	Configuration       string `json:"configuration"`
	ObserverSHA         string `json:"observer_sha256"`
	RuntimeConnection   string `json:"runtime_connection"`
}

func serviceIdentity() (uint32, uint32, error) {
	u, err := user.Lookup("nucleiron")
	if err != nil {
		return 0, 0, errRefused
	}
	uid, e := strconv.ParseUint(u.Uid, 10, 32)
	gid, g := strconv.ParseUint(u.Gid, 10, 32)
	if e != nil || g != nil || uid == 0 || gid == 0 {
		return 0, 0, errRefused
	}
	return uint32(uid), uint32(gid), nil
}

func pinDatabaseConfiguration(files protectedFiles, path string, gid uint32) (*os.File, []byte, error) {
	raw, err := files.read(path, 0640, 256*1024)
	if err != nil {
		return nil, nil, errRefused
	}
	f, err := openPath(path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, nil, errRefused
	}
	accepted := false
	defer func() {
		if !accepted {
			f.Close()
		}
	}()
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Gid != gid || metadata(f, files.uid, unix.S_IFREG|0640) != nil {
		return nil, nil, errRefused
	}
	again, err := io.ReadAll(io.LimitReader(f, 256*1024+1))
	if err != nil || !bytes.Equal(again, raw) || !samePath(f, path) {
		return nil, nil, errRefused
	}
	var config struct {
		Database struct {
			Backend string `toml:"backend"`
			SQLite  struct {
				File string `toml:"db_file"`
			} `toml:"sqlite3"`
		} `toml:"database"`
	}
	if _, err := toml.Decode(string(raw), &config); err != nil || config.Database.Backend != "sqlite3" || config.Database.SQLite.File != databasePath {
		return nil, nil, errRefused
	}
	accepted = true
	return f, raw, nil
}

type limitedOutput struct {
	bytes.Buffer
	limit int
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errRefused
	}
	return b.Buffer.Write(p)
}

func observeDatabase(ctx context.Context, r receipt) databaseObservation {
	bad := func(code string) databaseObservation {
		return databaseObservation{resourceObservation: unknown(code), Configuration: "unknown"}
	}
	uid, gid, err := serviceIdentity()
	if err != nil {
		return bad("database_service_identity")
	}
	config, configRaw, err := pinDatabaseConfiguration(protectedFiles{uid: 0, boundary: "/"}, controllerConfig, gid)
	if err != nil {
		return bad("database_configuration")
	}
	defer config.Close()
	expected, err := runningDigest()
	if err != nil {
		return bad("database_child")
	}
	// During fork/exec this intentional procfs magic link identifies the
	// child's inherited image, even when the deployed pathname was replaced.
	result, err := runDatabaseChild(ctx, "/proc/self/exe", []string{"--database-observer"}, r, uid, gid)
	if err != nil {
		return bad("database_child")
	}
	if result.ObserverSHA != expected {
		return bad("database_child_image")
	}
	var st unix.Stat_t
	if _, err := config.Seek(0, io.SeekStart); err != nil {
		return bad("database_configuration_changed")
	}
	again, err := io.ReadAll(io.LimitReader(config, 256*1024+1))
	if err != nil || !bytes.Equal(again, configRaw) || !samePath(config, controllerConfig) || (protectedFiles{uid: 0, boundary: "/"}).parents(controllerConfig) != nil || metadata(config, 0, unix.S_IFREG|0640) != nil || unix.Fstat(int(config.Fd()), &st) != nil || st.Gid != gid {
		return bad("database_configuration_changed")
	}
	result.Configuration = "current_file_fixed_database_verified"
	result.RuntimeConnection = "runtime_database_connection_unknown"
	return result
}

func runDatabaseChild(ctx context.Context, exe string, args []string, r receipt, uid, gid uint32) (databaseObservation, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return databaseObservation{}, err
	}
	raw, err := runDatabaseProcess(ctx, exe, args, data, uid, gid)
	var result databaseObservation
	if err != nil || strictObject(raw, &result) != nil || result.UID != uid || result.GID != gid || result.Groups == nil || len(result.Groups) != 0 {
		return result, errRefused
	}
	return result, nil
}

func runDatabaseProcess(ctx context.Context, exe string, args []string, data []byte, uid, gid uint32) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(data)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{}}, Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return unix.Kill(-cmd.Process.Pid, unix.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	output := &limitedOutput{limit: 32 * 1024}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if cmd.Run() != nil || ctx.Err() != nil || unix.Kill(-cmd.Process.Pid, 0) != unix.ESRCH {
		return nil, errRefused
	}
	return output.Bytes(), nil
}

func databaseWorker(ctx context.Context, args []string, input io.Reader, output io.Writer) int {
	if len(args) != 0 || !controlledEnvironment(os.Environ()) {
		return 1
	}
	if protectSessionSecrets() != nil {
		return 1
	}
	uid, gid, err := serviceIdentity()
	if err != nil || uint32(os.Getuid()) != uid || uint32(os.Geteuid()) != uid || uint32(os.Getgid()) != gid || uint32(os.Getegid()) != gid {
		return 1
	}
	groups, err := os.Getgroups()
	if err != nil || len(groups) != 0 {
		return 1
	}
	raw, err := io.ReadAll(io.LimitReader(input, 128*1024+1))
	if err != nil || len(raw) > 128*1024 {
		return 1
	}
	var r receipt
	if strictObject(raw, &r) != nil {
		return 1
	}
	encoded, _ := json.Marshal(r.Expected)
	expected, err := parseRequest(encoded)
	if err != nil || expected != r.Expected || !r.Observed.Guard.Valid() {
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result := readDatabase(ctx, databaseFiles{path: databasePath, boundary: "/", uid: uid, gid: gid}, r)
	// Getgroups may return nil for the verified empty OS group set; the
	// worker protocol uses an explicit empty array, never an absent/null fact.
	result.UID, result.GID, result.Groups = uid, gid, []int{}
	result.ObserverSHA, err = runningDigest()
	if err != nil {
		return 1
	}
	if json.NewEncoder(output).Encode(result) != nil {
		return 1
	}
	return 0
}

type databaseFiles struct {
	path, boundary string
	uid, gid       uint32
}
type databaseObject struct {
	file     *os.File
	path     string
	uid, gid uint32
	mode     uint32
}

func (o databaseObject) unchanged() bool {
	var st unix.Stat_t
	return samePath(o.file, o.path) && unix.Fstat(int(o.file.Fd()), &st) == nil && st.Uid == o.uid && st.Gid == o.gid && st.Mode == o.mode && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink == 1)
}

func (d databaseFiles) pin() (map[string]databaseObject, error) {
	objects := map[string]databaseObject{}
	closeAll := func() {
		for _, o := range objects {
			o.file.Close()
		}
	}
	add := func(path string, dir bool, optional bool) error {
		flags := unix.O_RDONLY
		if dir {
			flags |= unix.O_DIRECTORY
		}
		f, err := openPath(path, flags, 0)
		if optional && os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if unix.Fstat(int(f.Fd()), &st) != nil {
			f.Close()
			return errRefused
		}
		valid := st.Uid == d.uid && st.Gid == d.gid && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1 && st.Mode&0077 == 0
		if dir {
			valid = (st.Uid == 0 || st.Uid == d.uid) && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&0022 == 0
		}
		if !valid {
			f.Close()
			return errRefused
		}
		objects[path] = databaseObject{f, path, st.Uid, st.Gid, st.Mode}
		return nil
	}
	if !filepath.IsAbs(d.path) || filepath.Clean(d.path) != d.path {
		return nil, errRefused
	}
	for dir := filepath.Dir(d.path); ; dir = filepath.Dir(dir) {
		if add(dir, true, false) != nil {
			closeAll()
			return nil, errRefused
		}
		if dir == d.boundary {
			break
		}
		if dir == "/" {
			closeAll()
			return nil, errRefused
		}
	}
	for _, path := range []string{d.path, d.path + "-wal", d.path + "-shm"} {
		if add(path, false, path != d.path) != nil {
			closeAll()
			return nil, errRefused
		}
	}
	return objects, nil
}

func readDatabase(ctx context.Context, files databaseFiles, r receipt) databaseObservation {
	var result databaseObservation
	code := databaseSnapshot(ctx, files, func(tx *sql.Tx) error {
		var err error
		result, err = databaseRows(ctx, tx, r)
		return err
	})
	if code != "" {
		return databaseObservation{resourceObservation: unknown(code)}
	}
	return result
}

// Both job recovery and credential admission read one query-only snapshot and
// retain the database/WAL identities until the transaction has finished.
func databaseSnapshot(ctx context.Context, files databaseFiles, observe func(*sql.Tx) error) string {
	before, err := files.pin()
	if err != nil {
		return "database_files"
	}
	defer func() {
		for _, o := range before {
			o.file.Close()
		}
	}()
	u := url.URL{Scheme: "file", Path: files.path}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("_pragma", "query_only(1)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "database_open"
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return "database_connect"
	}
	defer conn.Close()
	var queryOnly int64
	if conn.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly) != nil || queryOnly != 1 {
		return "database_readonly"
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "database_transaction"
	}
	defer tx.Rollback()
	if err := databaseSchema(ctx, tx); err != nil {
		return "database_schema"
	}
	if observe(tx) != nil {
		return "database_values"
	}
	if ctx.Err() != nil || tx.Commit() != nil {
		return "database_transaction"
	}
	// Keep the connection alive through identity checks. SQLite may create SHM
	// as this service user; ordinary WAL content changes do not change identity.
	after, err := files.pin()
	if err != nil {
		return "database_files_changed"
	}
	defer func() {
		for _, o := range after {
			o.file.Close()
		}
	}()
	for path, o := range before {
		if !o.unchanged() {
			return "database_files_changed"
		}
		if _, ok := after[path]; !ok {
			return "database_files_changed"
		}
	}
	for _, o := range after {
		if !o.unchanged() {
			return "database_files_changed"
		}
	}
	return ""
}

type columnContract struct {
	name, kind string
	pk         bool
}

var databaseTables = map[string][]columnContract{
	"controller_infos": {{"id", "uuid", true}, {"controller_id", "text", false}, {"deleted_at", "datetime", false}},
	"repositories":     {{"id", "uuid", true}, {"owner", "text", false}, {"name", "text", false}, {"endpoint_name", "text", false}, {"deleted_at", "datetime", false}},
	"github_endpoints": {{"name", "varchar(64)", true}, {"endpoint_type", "text", false}, {"api_base_url", "text", false}, {"base_url", "text", false}, {"deleted_at", "datetime", false}},
	"scale_sets":       {{"id", "integer", true}, {"scale_set_id", "integer", false}, {"repo_id", "text", false}, {"name", "text", false}, {"provider_name", "text", false}, {"enabled", "numeric", false}, {"org_id", "text", false}, {"enterprise_id", "text", false}, {"deleted_at", "datetime", false}},
	"pools":            {{"id", "uuid", true}, {"enabled", "numeric", false}, {"deleted_at", "datetime", false}},
	"instances":        {{"id", "uuid", true}, {"name", "text", false}, {"provider_id", "text", false}, {"agent_id", "integer", false}, {"scale_set_fk_id", "integer", false}, {"pool_id", "text", false}, {"status", "text", false}, {"runner_status", "text", false}, {"deleted_at", "datetime", false}},
}

func databaseSchema(ctx context.Context, tx *sql.Tx) error {
	return databaseColumns(ctx, tx, databaseTables)
}

func databaseColumns(ctx context.Context, tx *sql.Tx, tables map[string][]columnContract) error {
	for table, columns := range tables {
		var kind string
		if tx.QueryRowContext(ctx, "SELECT type FROM sqlite_schema WHERE name=?", table).Scan(&kind) != nil || kind != "table" {
			return errRefused
		}
		rows, err := tx.QueryContext(ctx, "PRAGMA table_info('"+table+"')")
		if err != nil {
			return err
		}
		found := map[string]columnContract{}
		for rows.Next() {
			var cid, notnull, pk int64
			var name, kind string
			var value any
			if rows.Scan(&cid, &name, &kind, &notnull, &value, &pk) != nil {
				rows.Close()
				return errRefused
			}
			found[name] = columnContract{name, strings.ToLower(kind), pk > 0}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, column := range columns {
			if found[column.name] != column {
				return errRefused
			}
		}
	}
	return nil
}

func queryRows(ctx context.Context, tx *sql.Tx, query string, args ...any) ([][]any, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := [][]any{}
	for rows.Next() {
		if len(result) >= 10000 {
			return nil, errRefused
		}
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if rows.Scan(targets...) != nil {
			return nil, errRefused
		}
		result = append(result, values)
	}
	return result, rows.Err()
}

func stringValue(v any) (string, bool) { s, ok := v.(string); return s, ok }
func integer(v any) (int64, bool)      { i, ok := v.(int64); return i, ok }
func deletedFlag(v, kind any) (bool, bool) {
	n, ok := integer(v)
	k, ko := stringValue(kind)
	return n == 1, ok && ko && (n == 0 && k == "null" || n == 1 && k == "text")
}

func databaseRows(ctx context.Context, tx *sql.Tx, r receipt) (databaseObservation, error) {
	out := databaseObservation{resourceObservation: resource("observed", 0), Target: "absent_verified"}
	controller, err := queryRows(ctx, tx, "SELECT controller_id, deleted_at IS NOT NULL, typeof(deleted_at) FROM controller_infos")
	if err != nil || len(controller) != 1 {
		return out, errRefused
	}
	id, ok := stringValue(controller[0][0])
	deleted, valid := deletedFlag(controller[0][1], controller[0][2])
	if !ok || !valid || deleted || id != r.Expected.Controller {
		return out, errRefused
	}
	repo, err := queryRows(ctx, tx, "SELECT owner,name,endpoint_name,deleted_at IS NOT NULL,typeof(deleted_at) FROM repositories WHERE id=?", r.Expected.GARMRepository)
	if err != nil || len(repo) != 1 {
		return out, errRefused
	}
	owner, o := stringValue(repo[0][0])
	name, n := stringValue(repo[0][1])
	endpoint, e := stringValue(repo[0][2])
	deleted, valid = deletedFlag(repo[0][3], repo[0][4])
	if !o || !n || !e || !valid || deleted || owner != r.Expected.Owner || name != r.Expected.Repository || endpoint == "" {
		return out, errRefused
	}
	ep, err := queryRows(ctx, tx, "SELECT endpoint_type,api_base_url,base_url,deleted_at IS NOT NULL,typeof(deleted_at) FROM github_endpoints WHERE name=?", endpoint)
	if err != nil || len(ep) != 1 {
		return out, errRefused
	}
	kind, k := stringValue(ep[0][0])
	api, a := stringValue(ep[0][1])
	base, b := stringValue(ep[0][2])
	deleted, valid = deletedFlag(ep[0][3], ep[0][4])
	if !k || !a || !b || !valid || deleted || kind != "github" || strings.TrimSuffix(api, "/") != "https://api.github.com" || strings.TrimSuffix(base, "/") != "https://github.com" {
		return out, errRefused
	}
	scales, err := queryRows(ctx, tx, "SELECT id,scale_set_id,repo_id,name,provider_name,enabled,org_id,enterprise_id,deleted_at IS NOT NULL,typeof(deleted_at) FROM scale_sets")
	if err != nil {
		return out, err
	}
	found := false
	for _, s := range scales {
		id, ok := integer(s[0])
		enabled, en := integer(s[5])
		deleted, de := deletedFlag(s[8], s[9])
		if !ok || id <= 0 || id > 4294967295 || !en || (enabled != 0 && enabled != 1) || !de {
			return out, errRefused
		}
		if !deleted && enabled == 1 {
			out.EnabledScaleSets++
		}
		if id == int64(r.Expected.ScaleSet) {
			found = true
			github, gh := integer(s[1])
			repo, ro := stringValue(s[2])
			name, na := stringValue(s[3])
			provider, pr := stringValue(s[4])
			if deleted || !gh || github != int64(r.Observed.GitHubScaleSetID) || !ro || repo != r.Expected.GARMRepository || !na || name != r.Observed.Guard.ScaleSet || !pr || provider != "halo-lxd" || s[6] != nil || s[7] != nil {
				return out, errRefused
			}
		}
	}
	if !found {
		return out, errRefused
	}
	pools, err := queryRows(ctx, tx, "SELECT id,enabled,deleted_at IS NOT NULL,typeof(deleted_at) FROM pools")
	if err != nil {
		return out, err
	}
	for _, p := range pools {
		id, ok := stringValue(p[0])
		en, e := integer(p[1])
		deleted, d := deletedFlag(p[2], p[3])
		if !ok || !uuidPattern.MatchString(id) || !e || (en != 0 && en != 1) || !d {
			return out, errRefused
		}
		if !deleted && en == 1 {
			out.EnabledPools++
		}
	}
	instances, err := queryRows(ctx, tx, "SELECT id,name,provider_id,agent_id,scale_set_fk_id,pool_id,status,runner_status,deleted_at IS NOT NULL,typeof(deleted_at) FROM instances")
	if err != nil {
		return out, err
	}
	out.Count = len(instances)
	identityConflict := false
	for _, item := range instances {
		id, ok := stringValue(item[0])
		name, n := stringValue(item[1])
		provider, p := stringValue(item[2])
		agent, a := integer(item[3])
		scale, s := integer(item[4])
		status, st := stringValue(item[6])
		runner, ru := stringValue(item[7])
		deleted, de := deletedFlag(item[8], item[9])
		if !ok || !uuidPattern.MatchString(id) || !n || name == "" || !p || !a || agent <= 0 || !s || scale <= 0 || !st || status == "" || !ru || runner == "" || !de {
			return out, errRefused
		}
		if id != r.Expected.Instance && name != r.Observed.RunnerName {
			out.AdditionalInstances++
			continue
		}
		if id != r.Expected.Instance || name != r.Observed.RunnerName || provider != name || agent != r.Observed.RunnerID || scale != int64(r.Expected.ScaleSet) || item[5] != nil {
			identityConflict = true
		} else if deleted {
			out.State = "residual"
			out.Target = "soft_deleted"
		} else {
			out.Target = "present"
			switch status {
			case "running", "stopped", "error", "pending_delete", "pending_force_delete", "deleting", "deleted", "pending_create", "creating":
				out.TargetStatus = status
			default:
				return out, errRefused
			}
		}
	}
	if len(instances) > 1 {
		out.State = "residual"
	}
	// A same-name or same-ID conflict dominates an exact row in either query
	// order. An exact row's status cannot describe the conflicting identity.
	if identityConflict {
		out.State = "mismatch"
		out.Target = "mismatch"
		out.TargetStatus = ""
	}
	return out, nil
}
