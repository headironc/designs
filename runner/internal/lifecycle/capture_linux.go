// Package lifecycle owns execution identity receipts and their provenance.
package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/headironc/designs/runner/internal/provider"
)

var errRefused = errors.New("execution capture refused")
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var repoPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

type request struct {
	Version        int    `json:"version"`
	Operation      string `json:"operation_id"`
	RepositoryID   int64  `json:"repository_id"`
	Owner          string `json:"owner"`
	Repository     string `json:"repository"`
	RunID          int64  `json:"run_id"`
	Attempt        int    `json:"attempt"`
	JobID          int64  `json:"job_id"`
	Commit         string `json:"commit"`
	Controller     string `json:"controller_id"`
	GARMRepository string `json:"garm_repository_id"`
	ScaleSet       uint32 `json:"garm_scaleset_id"`
	Instance       string `json:"garm_instance_id"`
}

func parseRequest(raw []byte) (request, error) {
	var r request
	if strictObject(raw, &r) != nil || r.Version != 1 || !uuidPattern.MatchString(r.Operation) || r.RepositoryID <= 0 || !repoPart.MatchString(r.Owner) || !repoPart.MatchString(r.Repository) || r.Owner == "." || r.Owner == ".." || r.Repository == "." || r.Repository == ".." || r.RunID <= 0 || r.Attempt <= 0 || int64(r.Attempt) > 2147483647 || r.JobID <= 0 || !shaPattern.MatchString(r.Commit) || !uuidPattern.MatchString(r.Controller) || !uuidPattern.MatchString(r.GARMRepository) || r.ScaleSet == 0 || !uuidPattern.MatchString(r.Instance) {
		return r, errRefused
	}
	return r, nil
}

// Reject duplicate/case-aliased keys recursively, including SDK responses.
func unambiguous(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errRefused
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errRefused
				}
				name = strings.ToLower(name)
				if seen[name] {
					return errRefused
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errRefused
		}
		_, err = d.Token()
		return err
	}
	if value(0) != nil {
		return errRefused
	}
	if _, err := d.Token(); err != io.EOF {
		return errRefused
	}
	return nil
}

func strictObject(raw []byte, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' || unambiguous(raw) != nil {
		return errRefused
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil {
		return errRefused
	}
	for key := range keys {
		if key != strings.ToLower(key) {
			return errRefused
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

type observation struct {
	RunnerID         int64                     `json:"runner_id"`
	RunnerName       string                    `json:"runner_name"`
	GitHubScaleSetID int                       `json:"github_scaleset_id"`
	Guard            provider.ExecutionBinding `json:"guard"`
	VM               machine                   `json:"machine"`
	Host             process                   `json:"host"`
}

type receipt struct {
	Version     int         `json:"version"`
	ProducerSHA string      `json:"producer_sha256"`
	RequestSHA  string      `json:"request_sha256"`
	Expected    request     `json:"expected"`
	Observed    observation `json:"observed"`
	CapturedAt  string      `json:"captured_at"`
}

type capture struct {
	files       protectedFiles
	guard       func(context.Context, func(provider.ExecutionBinding, func() error) (func() error, error)) error
	platform    func(context.Context, request, provider.ExecutionBinding) (int, error)
	machine     func(context.Context, provider.ExecutionBinding) (machine, error)
	process     func(int64, string) (process, error)
	now         func() time.Time
	producerSHA string
}

func (c capture) execute(ctx context.Context, requestPath, operations string) (string, error) {
	raw, err := c.files.read(requestPath, 0600, 64*1024)
	if err != nil {
		return "request", err
	}
	r, err := parseRequest(raw)
	if err != nil {
		return "request", err
	}
	if ctx.Err() != nil {
		return "cancelled", ctx.Err()
	}
	if !digestPattern.MatchString(c.producerSHA) {
		return "producer", errRefused
	}
	if existing, err := c.files.original(operations, r, raw, c.producerSHA); err != nil {
		return "operation", err
	} else if existing {
		return "existing", nil
	}
	store, err := c.files.reserve(operations, r.Operation, raw)
	if err != nil {
		return "operation", err
	}
	defer store.close()
	stage := "guard"
	err = c.guard(ctx, func(b provider.ExecutionBinding, recheck func() error) (func() error, error) {
		if r.Controller != b.Controller || r.GARMRepository != b.Repository {
			return nil, errRefused
		}
		observe := func() (observation, error) {
			stage = "platform"
			scale, err := c.platform(ctx, r, b)
			if err != nil {
				return observation{}, err
			}
			stage = "machine"
			vm, err := c.machine(ctx, b)
			if err != nil {
				return observation{}, err
			}
			stage = "process"
			host, err := c.process(vm.PID, b.UUID)
			if err != nil {
				return observation{}, err
			}
			return observation{b.AgentID, b.Name, scale, b, vm, host}, nil
		}
		first, err := observe()
		if err != nil {
			return nil, err
		}
		second, err := observe()
		if err != nil {
			return nil, err
		}
		stage = "identity_changed"
		if first != second {
			return nil, errRefused
		}
		stage = "guard_changed"
		if recheck() != nil {
			return nil, errRefused
		}
		stage = "request_changed"
		current, err := c.files.read(requestPath, 0600, 64*1024)
		if err != nil || !bytes.Equal(current, raw) {
			return nil, errRefused
		}
		stage = "receipt"
		data, err := json.Marshal(receipt{1, c.producerSHA, fmt.Sprintf("%x", sha256.Sum256(raw)), r, second, c.now().UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return nil, err
		}
		stage = "guard_changed"
		return func() error {
			stage = "receipt"
			return store.commit(ctx, append(data, '\n'), func() error {
				if recheck() != nil {
					return errRefused
				}
				host, err := c.process(second.VM.PID, b.UUID)
				if err != nil || host != second.Host {
					return errRefused
				}
				return nil
			})
		}, nil
	})
	return stage, err
}

// Run exposes a fixed production observation route. Test endpoints and fixture
// roots are private implementation inputs, never flags or environment settings.
func Run(ctx context.Context, args []string, output, diagnostic io.Writer) int {
	stage := "arguments"
	fail := func() int {
		fmt.Fprintf(diagnostic, "{\"event\":\"execution_capture_refused\",\"stage\":%q}\n", stage)
		return 1
	}
	if len(args) == 0 || args[0] != "capture-execution" {
		return fail()
	}
	f := flag.NewFlagSet("capture-execution", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	requestPath := f.String("request", "", "protected trusted request")
	operations := f.String("operations", "", "protected root-owned receipt directory")
	githubCredential := f.String("github-token-file", "", "existing read-only credential")
	garmCredential := f.String("garm-token-file", "", "existing read-only credential")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || *requestPath == "" || *operations == "" || *githubCredential == "" || *garmCredential == "" {
		return fail()
	}
	stage = "identity"
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return fail()
	}
	stage = "environment"
	if !controlledEnvironment(os.Environ()) {
		return fail()
	}
	files := protectedFiles{uid: 0, boundary: "/"}
	stage = "credential"
	gh, err := files.read(*githubCredential, 0600, 64*1024+1)
	if err != nil {
		return fail()
	}
	ga, err := files.read(*garmCredential, 0600, 64*1024+1)
	if err != nil {
		return fail()
	}
	ghToken, gaToken := strings.TrimSpace(string(gh)), strings.TrimSpace(string(ga))
	if !validToken(ghToken) || !validToken(gaToken) {
		return fail()
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	p := newPlatforms(ghToken, gaToken, nil)
	defer p.close()
	stage = "producer"
	producerSHA, err := runningDigest()
	if err != nil {
		return fail()
	}
	c := capture{files: files, guard: provider.InspectExecution, platform: p.observe, machine: observeMachine, process: (hostFiles{proc: "/proc", cgroup: "/sys/fs/cgroup", realFS: true}).observe, now: time.Now, producerSHA: producerSHA}
	stage, err = c.execute(ctx, *requestPath, *operations)
	if err != nil {
		return fail()
	}
	event := "execution_identity_captured"
	if stage == "existing" {
		event = "execution_identity_original"
	}
	response := fmt.Sprintf("{\"event\":%q,\"version\":1}\n", event)
	if n, err := io.WriteString(output, response); err != nil || n != len(response) {
		stage = "response_delivery"
		return fail()
	}
	return 0
}

func controlledEnvironment(environment []string) bool {
	for _, e := range environment {
		key, value, _ := strings.Cut(e, "=")
		if value != "" && (strings.HasPrefix(key, "GH_") || strings.HasPrefix(key, "GITHUB_") || strings.HasPrefix(key, "GARM_") || strings.HasPrefix(key, "LXD_") || strings.HasSuffix(strings.ToUpper(key), "_PROXY") || key == "SSL_CERT_FILE" || key == "SSL_CERT_DIR" || key == "DEBUG" || key == "SWAGGER_DEBUG" || key == "GODEBUG" && strings.Contains(value, "http2debug=")) {
			return false
		}
	}
	return true
}

func validToken(s string) bool {
	if s == "" || len(s) > 64*1024 {
		return false
	}
	for _, c := range s {
		if c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}
