package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	lxd "github.com/canonical/lxd/client"
	"github.com/cloudbase/garm-provider-common/params"
	providerutil "github.com/cloudbase/garm/runner/providers/util"
)

type readPolicy struct {
	guardPolicy
	ProviderSHA string `json:"provider_sha256"`
	ConfigSHA   string `json:"config_sha256"`
}

type reader struct {
	invoke func(context.Context, *guardFiles, readPolicy, string, string) ([]byte, error)
	bound  func(context.Context, guardPolicy, activeInstance) error
	vacant func(context.Context) error
}

// Read implements normal-mode observations without changing guard records.
func Read(ctx context.Context, environment map[string]string, output, diagnostic io.Writer) int {
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
		stage, response, err = s.read(ctx, environment, reader{invoke: invokeRead, bound: boundRead, vacant: afterDelete})
	}
	if err == nil {
		stage = "response_delivery"
		n, writeErr := output.Write(response)
		err = writeErr
		if err == nil && n != len(response) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		return 0
	}
	if errors.Is(err, errBusy) {
		stage = "busy"
	}
	fmt.Fprintf(diagnostic, "{\"event\":\"guard_read_refused\",\"stage\":%q}\n", stage)
	if errors.Is(err, errBusy) {
		return 75
	}
	return 1
}

func (s startup) read(ctx context.Context, env map[string]string, r reader) (string, []byte, error) {
	phase := "admission"
	var response []byte
	stage, err := s.withState(func(g *guardFiles) error {
		var p readPolicy
		if decodeObject(g.policy, &p, true) != nil || !p.valid() || !normalState(g.raw) {
			return errRefused
		}
		command, name := env["GARM_COMMAND"], env["GARM_INSTANCE_ID"]
		if command != "GetVersion" && command != "GetInstance" && command != "ListInstances" {
			return errRefused
		}
		if env["GARM_CONTROLLER_ID"] != p.Controller || env["GARM_PROVIDER_CONFIG_FILE"] != s.config || env["GARM_INTERFACE_VERSION"] != "v0.1.0" {
			return errRefused
		}
		if (command == "ListInstances" && env["GARM_POOL_ID"] != p.Pool) || (env["GARM_POOL_ID"] != "" && env["GARM_POOL_ID"] != p.Pool) {
			return errRefused
		}
		var state guardState
		if decodeObject(g.raw, &state, false) != nil {
			return errRefused
		}
		var active activeInstance
		if !isNull(state.Active) && decodeObject(state.Active, &active, false) != nil {
			return errRefused
		}
		if command == "GetInstance" && (isNull(state.Active) || name != active.Name) {
			return errRefused
		}
		if command != "GetInstance" && name != "" {
			return errRefused
		}
		phase = "native"
		raw, err := r.invoke(ctx, g, p, command, name)
		if err != nil {
			return err
		}
		phase = "response"
		if command == "GetVersion" {
			// Native v0.1.0 returns a plain version token, not a JSON object.
			version := bytes.TrimSpace(raw)
			if len(version) == 0 {
				return errRefused
			}
			for _, b := range version {
				if b < 0x21 || b > 0x7e {
					return errRefused
				}
			}
		} else {
			var items []json.RawMessage
			if command == "GetInstance" {
				items = []json.RawMessage{raw}
			} else {
				if json.Unmarshal(raw, &items) != nil || items == nil {
					return errRefused
				}
			}
			if isNull(state.Active) {
				if len(items) != 0 {
					return errRefused
				}
				phase = "vacancy"
				if err := r.vacant(ctx); err != nil {
					return err
				}
			} else {
				if len(items) != 1 {
					return errRefused
				}
				var item params.ProviderInstance
				if decodeObject(items[0], &item, false) != nil || item.Name != active.Name || item.ProviderID != active.Name || item.OSType != params.Linux || item.OSArch != params.Amd64 || !providerutil.IsValidProviderStatus(item.Status) {
					return errRefused
				}
				phase = "instance"
				if err := r.bound(ctx, p.guardPolicy, active); err != nil {
					return err
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		response = raw
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

func invokeRead(ctx context.Context, g *guardFiles, p readPolicy, command, name string) ([]byte, error) {
	env := []string{"GARM_COMMAND=" + command, "GARM_CONTROLLER_ID=" + p.Controller}
	switch command {
	case "GetVersion":
	case "GetInstance":
		env = append(env, "GARM_INSTANCE_ID="+name)
	case "ListInstances":
		env = append(env, "GARM_POOL_ID="+p.Pool)
	default:
		return nil, errRefused
	}
	return invokeNative(ctx, g, p.ProviderSHA, p.ConfigSHA, env, nil)
}

func boundRead(ctx context.Context, p guardPolicy, active activeInstance) error {
	return withLXD(ctx, lxdSocket, func(client lxd.InstanceServer) error { return boundInstance(client, p, active) })
}

func boundInstance(client lxd.InstanceServer, p guardPolicy, active activeInstance) error {
	id, err := createdInstance(client, p, active.Name)
	if err != nil || id != active.UUID {
		return errRefused
	}
	return nil
}
