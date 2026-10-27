package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cloudbase/garm-provider-common/params"
)

type createPolicy struct {
	Controller  string `json:"controller_id"`
	Pool        string `json:"pool_id"`
	Image       string `json:"image_fingerprint"`
	Alias       string `json:"image_alias"`
	Specs       string `json:"extra_specs_sha256"`
	ProviderSHA string `json:"provider_sha256"`
	ConfigSHA   string `json:"config_sha256"`
}

type creator struct {
	capacity func(context.Context) error
	before   func(context.Context, createPolicy) error
	after    func(context.Context, createPolicy, string) (string, error)
	invoke   func(context.Context, *guardFiles, createPolicy, []byte) ([]byte, error)
	now      func() time.Time
}

// Create accepts only a new CreateInstance operation. It never replays an intent.
func Create(ctx context.Context, environment map[string]string, input io.Reader, output, diagnostic io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 330*time.Second)
	defer cancel()
	if closer, ok := output.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { closer.Close() })
		defer stop()
	}
	s, err := servicePaths()
	stage := "identity"
	var response []byte
	if err == nil {
		stage, response, err = s.create(ctx, environment, input, creator{
			capacity: hostCapacity, before: beforeCreate, after: afterCreate, invoke: invokeCreate, now: time.Now,
		})
	}
	if err == nil {
		var n int
		n, err = output.Write(response)
		if err == nil && n != len(response) {
			err = io.ErrShortWrite
		}
		stage = "response_delivery"
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, errBusy) {
		stage = "busy"
	}
	fmt.Fprintf(diagnostic, "{\"event\":\"guard_create_refused\",\"stage\":%q}\n", stage)
	if errors.Is(err, errBusy) {
		return 75
	}
	return 1
}

func (s startup) create(ctx context.Context, env map[string]string, input io.Reader, c creator) (string, []byte, error) {
	if closer, ok := input.(io.ReadCloser); ok {
		// This command owns stdin. Closing it releases a pipe read on cancellation.
		stop := context.AfterFunc(ctx, func() { closer.Close() })
		defer stop()
	}
	var response []byte
	phase := "admission"
	stage, err := s.withState(func(g *guardFiles) error {
		var p createPolicy
		if decodeObject(g.policy, &p, true) != nil || !uuid.MatchString(p.Controller) || !uuid.MatchString(p.Pool) || !digest.MatchString(p.Image) || !digest.MatchString(p.Specs) || p.Alias == "" {
			return errRefused
		}
		if env["GARM_COMMAND"] != "CreateInstance" || env["GARM_CONTROLLER_ID"] != p.Controller || env["GARM_POOL_ID"] != p.Pool || env["GARM_PROVIDER_CONFIG_FILE"] != s.config || env["GARM_INTERFACE_VERSION"] != "v0.1.0" {
			return errRefused
		}
		var state guardState
		if !normalState(g.raw) || decodeObject(g.raw, &state, false) != nil || !isNull(state.Active) {
			return errRefused
		}
		phase = "input"
		raw, err := io.ReadAll(io.LimitReader(input, 2*1024*1024+1))
		if err != nil || len(raw) > 2*1024*1024 {
			return errRefused
		}
		request, err := createInput(raw, p)
		if err != nil {
			return err
		}
		phase = "capacity"
		if err := c.capacity(ctx); err != nil {
			return err
		}
		phase = "inventory"
		if err := c.before(ctx, p); err != nil {
			return err
		}
		phase = "intent"
		state.Intent, _ = json.Marshal(struct {
			Command string `json:"command"`
			Name    string `json:"name"`
			At      int64  `json:"at"`
		}{"CreateInstance", request.Name, c.now().Unix()})
		next, _ := json.Marshal(state)
		if err := g.persist(ctx, next, g.dir.Sync); err != nil {
			return err
		}
		// From here every failure retains the original durable intent, including
		// a failed exec or a lost response. No path automatically retries.
		phase = "native"
		result, err := c.invoke(ctx, g, p, raw)
		if err != nil {
			return err
		}
		phase = "response"
		var instance params.ProviderInstance
		if decodeObject(result, &instance, false) != nil || instance.Name != request.Name || instance.ProviderID != request.Name || instance.Status != params.InstanceRunning || instance.OSType != params.Linux || instance.OSArch != params.Amd64 || len(instance.ProviderFault) != 0 {
			return errRefused
		}
		phase = "instance"
		id, err := c.after(ctx, p, request.Name)
		if err != nil || !uuid.MatchString(id) {
			return errRefused
		}
		phase = "active"
		ready := false
		state.Active, _ = json.Marshal(activeInstance{Name: request.Name, UUID: id, Ready: &ready, At: json.Number(fmt.Sprint(c.now().Unix()))})
		state.Intent = json.RawMessage("null")
		next, _ = json.Marshal(state)
		if err := g.persist(ctx, next, g.dir.Sync); err != nil {
			return err
		}
		response = result
		return nil
	})
	if err != nil {
		response = nil
		if stage == "state" {
			stage = phase
		}
	}
	return stage, response, err
}

func createInput(raw []byte, p createPolicy) (params.BootstrapInstance, error) {
	var request params.BootstrapInstance
	if decodeObject(raw, &request, false) != nil || !instanceName.MatchString(request.Name) || request.Image != p.Alias || request.Flavor != "halo-ci" || request.OSType != params.Linux || request.OSArch != params.Amd64 || request.PoolID != p.Pool || !request.JitConfigEnabled || len(request.SSHKeys) != 0 || request.RepoURL != "https://github.com/silence-force/nucleus" || request.CallbackURL != "https://10.203.64.1:9443/api/v1/callbacks" || request.MetadataURL != "https://10.203.64.1:9443/api/v1/metadata" || request.InstanceToken == "" {
		return request, errRefused
	}
	// The deployed producer owns exactly these two fields. Its template is
	// base64 ASCII, so this fixed encoding matches Python sort_keys/separators
	// without inventing a general cross-language JSON canonicalizer.
	var specs struct {
		DisableUpdates *bool  `json:"disable_updates"`
		Template       string `json:"runner_install_template"`
	}
	if decodeObject(request.ExtraSpecs, &specs, false) != nil || specs.DisableUpdates == nil || !*specs.DisableUpdates || specs.Template == "" {
		return request, errRefused
	}
	template, err := base64.StdEncoding.Strict().DecodeString(specs.Template)
	if err != nil || base64.StdEncoding.EncodeToString(template) != specs.Template {
		return request, errRefused
	}
	encoded, err := json.Marshal(specs)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(encoded)) != p.Specs {
		return request, errRefused
	}
	// No reserialization of the request sent to native; bootstrap bytes and
	// credentials stay in memory and never enter intent records or diagnostics.
	return request, nil
}
