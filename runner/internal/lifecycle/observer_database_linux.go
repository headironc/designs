package lifecycle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type observerDatabaseRequest struct {
	Plan    observerPlan `json:"plan"`
	Session sessionPlan  `json:"session"`
}

type observerDatabase struct {
	Session  sessionDatabase `json:"session"`
	Payload  []byte          `json:"payload"`
	Identity string          `json:"identity_sha256"`
	Source   string          `json:"credential_source_sha256"`
	UID      uint32          `json:"uid"`
	GID      uint32          `json:"gid"`
	Groups   []int           `json:"groups"`
	Producer string          `json:"observer_sha256"`
}

func observerDatabaseRows(ctx context.Context, tx *sql.Tx, p observerPlan, sp sessionPlan) (observerDatabase, error) {
	out := observerDatabase{}
	user, err := sessionDatabaseRows(ctx, tx, sp)
	if err != nil || user.Controller != p.Controller || databaseColumns(ctx, tx, map[string][]columnContract{
		"github_credentials": {{"id", "integer", true}, {"name", "varchar(64)", false}, {"user_id", "uuid", false}, {"auth_type", "text", false}, {"payload", "longblob", false}, {"endpoint_name", "text", false}, {"updated_at", "datetime", false}, {"deleted_at", "datetime", false}},
	}) != nil {
		return out, errRefused
	}
	rows, err := queryRows(ctx, tx, "SELECT id,name,user_id,auth_type,payload,endpoint_name,CAST(updated_at AS TEXT),deleted_at IS NOT NULL,typeof(deleted_at) FROM github_credentials WHERE id=?", p.Credential)
	if err != nil || len(rows) != 1 {
		return out, errRefused
	}
	r := rows[0]
	id, idOK := integer(r[0])
	name, nameOK := stringValue(r[1])
	auth, authOK := stringValue(r[3])
	payload, payloadOK := r[4].([]byte)
	ep, epOK := stringValue(r[5])
	updated, updatedOK := stringValue(r[6])
	deleted, valid := deletedFlag(r[7], r[8])
	if !idOK || id != p.Credential || !nameOK || name != p.CredentialName || !authOK || auth != "app" || !payloadOK || len(payload) > 8192 || len(payload) == 0 || bytesSHA(payload) != p.PayloadSHA || !epOK || ep != p.Endpoint || !updatedOK || updated != p.Updated || !valid || deleted {
		return out, errRefused
	}
	if r[2] != nil {
		owner, ok := stringValue(r[2])
		if !ok || owner != user.User {
			return out, errRefused
		}
	}
	endpoints, err := queryRows(ctx, tx, "SELECT endpoint_type,api_base_url,base_url,deleted_at IS NOT NULL,typeof(deleted_at) FROM github_endpoints WHERE name=?", p.Endpoint)
	if err != nil || len(endpoints) != 1 {
		return out, errRefused
	}
	e := endpoints[0]
	deleted, valid = deletedFlag(e[3], e[4])
	if e[0] != "github" || e[1] != "https://api.github.com" && e[1] != "https://api.github.com/" || e[2] != "https://github.com" && e[2] != "https://github.com/" || !valid || deleted {
		return out, errRefused
	}
	// Include credential ownership and endpoint fields in the snapshot identity.
	raw, err := json.Marshal(struct {
		User                 string
		Credential, Endpoint []any
	}{user.Identity, r, e})
	if err != nil {
		return out, errRefused
	}
	stable, err := json.Marshal(struct {
		Controller, User     string
		Generation           int64
		Credential, Endpoint []any
	}{user.Controller, user.User, user.Generation, r, e})
	if err != nil {
		return out, errRefused
	}
	return observerDatabase{Session: user, Payload: append([]byte(nil), payload...), Identity: bytesSHA(raw), Source: bytesSHA(stable)}, nil
}

func readObserverDatabase(ctx context.Context, files databaseFiles, p observerPlan, sp sessionPlan) (observerDatabase, error) {
	var out observerDatabase
	if databaseSnapshot(ctx, files, func(tx *sql.Tx) error { var err error; out, err = observerDatabaseRows(ctx, tx, p, sp); return err }) != "" {
		clear(out.Payload)
		return observerDatabase{}, errRefused
	}
	return out, nil
}

func observeObserverDatabase(ctx context.Context, p observerPlan, sp sessionPlan) (observerDatabase, error) {
	uid, gid, err := serviceIdentity()
	if err != nil {
		return observerDatabase{}, errRefused
	}
	files := protectedFiles{uid: 0, boundary: "/"}
	config, before, err := pinDatabaseConfiguration(files, controllerConfig, gid)
	if err != nil {
		return observerDatabase{}, errRefused
	}
	defer config.Close()
	defer clear(before)
	producer, err := runningDigest()
	if err != nil {
		return observerDatabase{}, errRefused
	}
	input, _ := json.Marshal(observerDatabaseRequest{p, sp})
	raw, err := runDatabaseProcess(ctx, "/proc/self/exe", []string{"--github-observer-database"}, input, uid, gid)
	defer clear(raw)
	var out observerDatabase
	if err != nil || strictObject(raw, &out) != nil || out.UID != uid || out.GID != gid || out.Groups == nil || len(out.Groups) != 0 || out.Producer != producer || out.Session.Controller != p.Controller || !digestPattern.MatchString(out.Identity) || !digestPattern.MatchString(out.Source) || bytesSHA(out.Payload) != p.PayloadSHA {
		clear(out.Payload)
		return observerDatabase{}, errRefused
	}
	var st unix.Stat_t
	if _, err := config.Seek(0, io.SeekStart); err != nil {
		clear(out.Payload)
		return observerDatabase{}, errRefused
	}
	after, err := io.ReadAll(io.LimitReader(config, 256*1024+1))
	defer clear(after)
	if err != nil || !bytes.Equal(before, after) || !samePath(config, controllerConfig) || files.parents(controllerConfig) != nil || metadata(config, 0, unix.S_IFREG|0640) != nil || unix.Fstat(int(config.Fd()), &st) != nil || st.Gid != gid {
		clear(out.Payload)
		return observerDatabase{}, errRefused
	}
	return out, nil
}

func observerDatabaseWorker(ctx context.Context, args []string, input io.Reader, output io.Writer) int {
	if len(args) != 0 || !controlledEnvironment(os.Environ()) || protectSessionSecrets() != nil {
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
	raw, err := io.ReadAll(io.LimitReader(input, 64*1024+1))
	if err != nil {
		return 1
	}
	var request observerDatabaseRequest
	if strictObject(raw, &request) != nil {
		return 1
	}
	pb, _ := json.Marshal(request.Plan)
	spb, _ := json.Marshal(request.Session)
	p, err := decodeObserverPlan(pb)
	if err != nil {
		return 1
	}
	sp, err := decodeSessionPlan(spb)
	if err != nil || sp.Controller != p.Controller {
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := readObserverDatabase(ctx, databaseFiles{path: databasePath, boundary: "/", uid: uid, gid: gid}, p, sp)
	if err != nil {
		return 1
	}
	defer clear(out.Payload)
	out.UID, out.GID, out.Groups = uid, gid, []int{}
	out.Producer, err = runningDigest()
	if err != nil || json.NewEncoder(output).Encode(out) != nil {
		return 1
	}
	return 0
}
