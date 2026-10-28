package lifecycle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// This is a pre-job observation, not an execution receipt or a judgment about
// historical operations. No password or password-derived value leaves SQLite.
type sessionDatabase struct {
	Controller  string `json:"controller_id"`
	User        string `json:"user_id"`
	Generation  int64  `json:"generation"`
	Identity    string `json:"identity_sha256"`
	UID         uint32 `json:"uid"`
	GID         uint32 `json:"gid"`
	Groups      []int  `json:"groups"`
	ObserverSHA string `json:"observer_sha256"`
}

func sessionDatabaseRows(ctx context.Context, tx *sql.Tx, p sessionPlan) (sessionDatabase, error) {
	out := sessionDatabase{}
	// GARM's Username tag does not specify a SQL type; SQLite uses text.
	if databaseColumns(ctx, tx, map[string][]columnContract{
		"users": {{"id", "uuid", true}, {"username", "text", false}, {"generation", "integer", false}, {"is_admin", "numeric", false}, {"enabled", "numeric", false}, {"deleted_at", "datetime", false}},
	}) != nil {
		return out, errRefused
	}
	controller, err := queryRows(ctx, tx, "SELECT controller_id,deleted_at IS NOT NULL,typeof(deleted_at) FROM controller_infos")
	if err != nil || len(controller) != 1 {
		return out, errRefused
	}
	id, ok := stringValue(controller[0][0])
	deleted, valid := deletedFlag(controller[0][1], controller[0][2])
	if !ok || id != p.Controller || !valid || deleted {
		return out, errRefused
	}
	out.Controller = id
	users, err := queryRows(ctx, tx, "SELECT id,username,generation,is_admin,enabled,deleted_at IS NOT NULL,typeof(deleted_at) FROM users")
	if err != nil || len(users) != 1 {
		return out, errRefused
	}
	u := users[0]
	out.User, ok = stringValue(u[0])
	name, named := stringValue(u[1])
	var generated bool
	out.Generation, generated = integer(u[2])
	admin, a := integer(u[3])
	enabled, e := integer(u[4])
	deleted, valid = deletedFlag(u[5], u[6])
	if !ok || !uuidPattern.MatchString(out.User) || !named || name != p.Username || !generated || out.Generation < 0 || !a || admin != 1 || !e || enabled != 1 || !valid || deleted {
		return sessionDatabase{}, errRefused
	}
	facts := []string{out.Controller, out.User, name, strconv.FormatInt(out.Generation, 10)}
	// Even tombstones are conservatively retained as blockers here. They need
	// their original recovery owner, not a credential window's interpretation.
	instances, err := queryRows(ctx, tx, "SELECT id FROM instances")
	if err != nil || len(instances) != 0 {
		return sessionDatabase{}, errRefused
	}
	for _, table := range []string{"scale_sets", "pools"} {
		rows, err := queryRows(ctx, tx, "SELECT id,enabled,deleted_at IS NOT NULL,typeof(deleted_at) FROM "+table)
		if err != nil {
			return sessionDatabase{}, errRefused
		}
		seen := map[string]bool{}
		for _, row := range rows {
			key := ""
			if table == "scale_sets" {
				n, ok := integer(row[0])
				if !ok || n <= 0 || n > 4294967295 {
					return sessionDatabase{}, errRefused
				}
				key = strconv.FormatInt(n, 10)
			} else {
				var ok bool
				key, ok = stringValue(row[0])
				if !ok || !uuidPattern.MatchString(key) {
					return sessionDatabase{}, errRefused
				}
			}
			enabled, ok := integer(row[1])
			deleted, valid := deletedFlag(row[2], row[3])
			if !ok || enabled != 0 || !valid || seen[key] {
				return sessionDatabase{}, errRefused
			}
			seen[key] = true
			facts = append(facts, table+"/"+key+"/"+strconv.FormatBool(deleted))
		}
	}
	sort.Strings(facts)
	raw, _ := json.Marshal(facts)
	out.Identity = bytesSHA(raw)
	return out, nil
}

func readSessionDatabase(ctx context.Context, files databaseFiles, p sessionPlan) (sessionDatabase, error) {
	var result sessionDatabase
	code := databaseSnapshot(ctx, files, func(tx *sql.Tx) error {
		var err error
		result, err = sessionDatabaseRows(ctx, tx, p)
		return err
	})
	if code != "" {
		return sessionDatabase{}, errRefused
	}
	return result, nil
}

func observeSessionDatabase(ctx context.Context, p sessionPlan) (sessionDatabase, error) {
	uid, gid, err := serviceIdentity()
	if err != nil {
		return sessionDatabase{}, errRefused
	}
	files := protectedFiles{uid: 0, boundary: "/"}
	config, before, err := pinDatabaseConfiguration(files, controllerConfig, gid)
	if err != nil {
		return sessionDatabase{}, errRefused
	}
	defer config.Close()
	expected, err := runningDigest()
	if err != nil {
		return sessionDatabase{}, errRefused
	}
	data, err := json.Marshal(p)
	if err != nil {
		return sessionDatabase{}, errRefused
	}
	raw, err := runDatabaseProcess(ctx, "/proc/self/exe", []string{"--session-database-observer"}, data, uid, gid)
	var result sessionDatabase
	if err != nil || strictObject(raw, &result) != nil || result.UID != uid || result.GID != gid || result.Groups == nil || len(result.Groups) != 0 || result.ObserverSHA != expected || result.Controller != p.Controller || !uuidPattern.MatchString(result.User) || result.Generation < 0 || !digestPattern.MatchString(result.Identity) {
		return sessionDatabase{}, errRefused
	}
	var st unix.Stat_t
	if _, err := config.Seek(0, io.SeekStart); err != nil {
		return sessionDatabase{}, errRefused
	}
	again, err := io.ReadAll(io.LimitReader(config, 256*1024+1))
	if err != nil || !bytes.Equal(before, again) || !samePath(config, controllerConfig) || files.parents(controllerConfig) != nil || metadata(config, 0, unix.S_IFREG|0640) != nil || unix.Fstat(int(config.Fd()), &st) != nil || st.Gid != gid {
		return sessionDatabase{}, errRefused
	}
	return result, nil
}

func sessionDatabaseWorker(ctx context.Context, args []string, input io.Reader, output io.Writer) int {
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
	raw, err := io.ReadAll(io.LimitReader(input, 64*1024+1))
	if err != nil {
		return 1
	}
	p, err := decodeSessionPlan(raw)
	if err != nil {
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := readSessionDatabase(ctx, databaseFiles{path: databasePath, boundary: "/", uid: uid, gid: gid}, p)
	if err != nil {
		return 1
	}
	result.UID, result.GID, result.Groups = uid, gid, []int{}
	result.ObserverSHA, err = runningDigest()
	if err != nil || json.NewEncoder(output).Encode(result) != nil {
		return 1
	}
	return 0
}
