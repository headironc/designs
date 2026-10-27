//go:build linux

package watchdog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	garm "github.com/cloudbase/garm/client"
	"github.com/cloudbase/garm/client/controller_info"
	"github.com/cloudbase/garm/client/login"
	"github.com/cloudbase/garm/client/repositories"
	"github.com/cloudbase/garm/client/scalesets"
	"github.com/cloudbase/garm/params"
	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
)

type controller struct {
	client *http.Client
	api    *garm.GarmAPI
}

func newController(host string) *controller {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	rt := httptransport.NewWithClient(host, "/api/v1", []string{"http"}, client)
	// The SDK otherwise inherits DEBUG/SWAGGER_DEBUG and can dump credentials.
	rt.Debug = false
	rt.Consumers = map[string]runtime.Consumer{"application/json": runtime.ConsumerFunc(consumeResponse)}
	return &controller{client: client, api: garm.New(rt, nil)}
}

// Keep the response bound and reject malformed booleans before the SDK's bool
// fields can turn an explicit null into a successful disabled observation.
func consumeResponse(body io.Reader, target any) error {
	data, err := io.ReadAll(io.LimitReader(body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 {
		return errRefused
	}
	type enabledResponse struct {
		Enabled enabledFlag `json:"enabled"`
	}
	switch target.(type) {
	case *params.ScaleSet:
		var wire enabledResponse
		if json.Unmarshal(data, &wire) != nil {
			return errRefused
		}
	case *params.ScaleSets:
		var wire []enabledResponse
		if json.Unmarshal(data, &wire) != nil || wire == nil {
			return errRefused
		}
	}
	if json.Unmarshal(data, target) != nil {
		return errRefused
	}
	return nil
}

func (c *controller) login(ctx context.Context, password []byte) (string, error) {
	value := strings.TrimSpace(string(password))
	if value == "" {
		return "", errRefused
	}
	response, err := c.api.Login.Login(login.NewLoginParamsWithContext(ctx).WithBody(params.PasswordLoginParams{Username: "haloadmin", Password: value}), nil)
	if err != nil || response == nil || response.Payload.Token == "" || strings.ContainsAny(response.Payload.Token, "\r\n") {
		return "", errRefused
	}
	return response.Payload.Token, nil
}

func (c *controller) identity(ctx context.Context, token, expected string) error {
	info, err := c.api.ControllerInfo.ControllerInfo(controller_info.NewControllerInfoParamsWithContext(ctx), httptransport.BearerToken(token))
	if err != nil || info == nil || info.Payload.ControllerID.String() != expected {
		return errRefused
	}
	return nil
}

type scaleSet struct {
	ID         int64
	Repository string
	Name       string
	Provider   string
	Enabled    bool
}

// GARM v0.2.1 omits false (params.ScaleSet.Enabled has omitempty).
// Absence therefore means disabled, while a supplied null is malformed.
type enabledFlag bool

func (v *enabledFlag) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true":
		*v = true
	case "false":
		*v = false
	default:
		return errRefused
	}
	return nil
}

func (s scaleSet) matches(b binding, id int64) bool {
	// The GARM route parses this ID as uint32 even on a 64-bit host.
	return id > 0 && id <= 4294967295 && s.ID == id && s.Repository == b.Repository && s.Name == b.ScaleSet && s.Provider == "halo-lxd"
}

func observedScaleSet(s params.ScaleSet) (scaleSet, error) {
	if s.ID > 4294967295 {
		return scaleSet{}, errRefused
	}
	return scaleSet{ID: int64(s.ID), Repository: s.RepoID, Name: s.Name, Provider: s.ProviderName, Enabled: s.Enabled}, nil
}

func (c *controller) scaleSets(ctx context.Context, token, repository string) ([]scaleSet, error) {
	response, err := c.api.Repositories.ListRepoScaleSets(repositories.NewListRepoScaleSetsParamsWithContext(ctx).WithRepoID(repository), httptransport.BearerToken(token))
	if err != nil || response == nil || response.Payload == nil {
		return nil, errRefused
	}
	result := make([]scaleSet, 0, len(response.Payload))
	for _, item := range response.Payload {
		observed, err := observedScaleSet(item)
		if err != nil {
			return nil, err
		}
		result = append(result, observed)
	}
	return result, nil
}

func (c *controller) scaleSet(ctx context.Context, token string, id int64) (scaleSet, error) {
	response, err := c.api.Scalesets.GetScaleSet(scalesets.NewGetScaleSetParamsWithContext(ctx).WithScalesetID(strconv.FormatInt(id, 10)), httptransport.BearerToken(token))
	if err != nil || response == nil {
		return scaleSet{}, errRefused
	}
	return observedScaleSet(response.Payload)
}

func (c *controller) disable(ctx context.Context, token string, id int64) error {
	enabled := false
	_, err := c.api.Scalesets.UpdateScaleSet(scalesets.NewUpdateScaleSetParamsWithContext(ctx).
		WithScalesetID(strconv.FormatInt(id, 10)).WithBody(params.UpdateScaleSetParams{Enabled: &enabled}), httptransport.BearerToken(token))
	if err != nil {
		return errRefused
	}
	return nil
}
