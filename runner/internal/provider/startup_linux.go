// Package provider owns admission to the guarded native provider.
package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var errRefused = errors.New("startup refused")
var errBusy = errors.New("provider operation in progress")
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var instanceName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type startup struct {
	policy, native, config, state, maintenance string
	rootUID, serviceUID                        uint32
}

// CheckStartup only checks normal startup; it never invokes a provider or writes state.
func CheckStartup(output io.Writer) int {
	s, err := servicePaths()
	if err != nil {
		return refused(output, "identity")
	}
	stage, err := s.check()
	if errors.Is(err, errBusy) {
		fmt.Fprintln(output, `{"event":"guard_startup_refused","stage":"busy"}`)
		return 75
	}
	if err != nil {
		return refused(output, stage)
	}
	return 0
}

func servicePaths() (startup, error) {
	identity, err := user.Lookup("nucleiron")
	if err != nil {
		return startup{}, errRefused
	}
	uid, uidErr := strconv.ParseUint(identity.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(identity.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || uint64(os.Getuid()) != uid || uint64(os.Geteuid()) != uid || uint64(os.Getegid()) != gid {
		return startup{}, errRefused
	}
	s := startup{
		policy: "/etc/nucleus-halo/provider-policy.json", native: "/opt/nucleus-halo/bin/garm-provider-lxd",
		config: "/etc/nucleus-halo/garm-provider-lxd.toml", state: "/var/lib/nucleiron/provider-guard",
		maintenance: "/run/nucleiron-records-only", serviceUID: uint32(uid),
	}
	return s, nil
}

func refused(output io.Writer, stage string) int {
	// Only fixed stage names reach the journal; file contents and errors may contain secrets.
	fmt.Fprintf(output, "{\"event\":\"guard_startup_refused\",\"stage\":%q}\n", stage)
	return 1
}

func (s startup) check() (string, error) {
	return s.withState(func(g *guardFiles) error {
		if !normalState(g.raw) {
			return errRefused
		}
		return nil
	})
}

type guardFiles struct {
	s               startup
	dir, lock, file *os.File
	raw, policy     []byte
}

func (g *guardFiles) unchanged() bool {
	return samePath(g.dir, g.s.state) && sameAt(g.lock, g.dir, "operation.lock") && sameAt(g.file, g.dir, "state.json")
}

func (s startup) withState(check func(*guardFiles) error) (string, error) {
	return s.withStatePublication(check, nil)
}

func (s startup) withStatePublication(check func(*guardFiles) error, publish func() error) (string, error) {
	if err := absent(s.maintenance); err != nil {
		return "maintenance", err
	}
	dir, err := openPath(s.state, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return "state_directory", err
	}
	defer dir.Close()
	if err := metadata(dir, s.serviceUID, unix.S_IFDIR|0700); err != nil {
		return "state_directory", err
	}
	// The permanent lock is provisioned with state, never created or replaced by a check.
	lock, err := openAt(dir, "operation.lock")
	if err != nil {
		return "lock", err
	}
	defer lock.Close()
	if err := metadata(lock, s.serviceUID, unix.S_IFREG|0600); err != nil {
		return "lock", err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return "busy", errBusy
		}
		return "lock", err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := absent(s.maintenance); err != nil {
		return "maintenance", err
	}
	// An interrupted local state transaction is not a fresh health check.
	for _, name := range []string{"state.pending", "state.new"} {
		if err := absentAt(dir, name); err != nil {
			return "state_transaction", err
		}
	}
	raw, err := readPath(s.policy, s.rootUID, 0644, 64*1024)
	var policy struct {
		ProviderSHA string `json:"provider_sha256"`
		ConfigSHA   string `json:"config_sha256"`
	}
	// Other policy fields belong to provider writes and the watchdog.
	if err != nil || decodeObject(raw, &policy, true) != nil || !digest.MatchString(policy.ProviderSHA) || !digest.MatchString(policy.ConfigSHA) {
		return "policy", errRefused
	}
	if err := checkDigest(s.native, s.rootUID, 0755, policy.ProviderSHA); err != nil {
		return "provider_digest", err
	}
	if err := checkDigest(s.config, s.rootUID, 0644, policy.ConfigSHA); err != nil {
		return "config_digest", err
	}
	policyRaw := raw
	f, err := openAt(dir, "state.json")
	if err != nil {
		return "state", err
	}
	g := &guardFiles{s: s, dir: dir, lock: lock, file: f, policy: policyRaw}
	defer func() { g.file.Close() }()
	raw, err = readFile(f, s.serviceUID, 0600, 64*1024)
	if err != nil {
		return "state", errRefused
	}
	g.raw = raw
	if err := check(g); err != nil {
		return "state", err
	}
	// Detect path replacement while checking pinned descriptors, before reporting admission.
	if !g.unchanged() {
		return "state_changed", errRefused
	}
	if err := absent(s.maintenance); err != nil {
		return "maintenance", err
	}
	if publish != nil {
		if err := publish(); err != nil {
			return "publication", err
		}
	}
	return "", nil
}

func normalState(raw []byte) bool {
	var state struct {
		Version int             `json:"version"`
		Active  json.RawMessage `json:"active"`
		Intent  json.RawMessage `json:"intent"`
		Latch   json.RawMessage `json:"latch"`
	}
	if decodeObject(raw, &state, false) != nil || state.Version != 1 || !isNull(state.Intent) || !isNull(state.Latch) {
		return false
	}
	if isNull(state.Active) {
		return true
	}
	var active struct {
		Name    string  `json:"name"`
		UUID    string  `json:"uuid"`
		Ready   *bool   `json:"ready"`
		At      float64 `json:"at"`
		AgentID *int64  `json:"agent_id"`
	}
	if decodeObject(state.Active, &active, false) != nil || !instanceName.MatchString(active.Name) || !uuid.MatchString(active.UUID) || active.Ready == nil || active.At <= 0 || math.IsInf(active.At, 0) {
		return false
	}
	// An active instance still owns capacity. Startup does not clear it or authorize another VM.
	return (!*active.Ready && active.AgentID == nil) || (*active.Ready && active.AgentID != nil && *active.AgentID > 0)
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func decodeObject(raw []byte, target any, allowUnknown bool) error {
	// encoding/json accepts repeated keys. Refuse ambiguity in admission-bearing objects.
	keys := json.NewDecoder(bytes.NewReader(raw))
	first, err := keys.Token()
	if err != nil || first != json.Delim('{') {
		return errRefused
	}
	seen := make(map[string]bool)
	for keys.More() {
		key, err := keys.Token()
		name, ok := key.(string)
		if err != nil || !ok || name != strings.ToLower(name) || seen[name] {
			return errRefused
		}
		seen[name] = true
		if keys.Decode(new(json.RawMessage)) != nil {
			return errRefused
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if !allowUnknown {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errRefused
	}
	return nil
}

func checkDigest(path string, uid uint32, mode uint32, expected string) error {
	f, err := openPath(path, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := metadata(f, uid, unix.S_IFREG|mode); err != nil {
		return err
	}
	before, err := f.Stat()
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !samePath(f, path) || hex.EncodeToString(h.Sum(nil)) != expected {
		return errRefused
	}
	return nil
}
