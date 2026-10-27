//go:build linux

package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type controller struct {
	client   *http.Client
	endpoint string
}

func newController() *controller {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &controller{endpoint: "http://127.0.0.1:9997/api/v1", client: &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *controller) request(ctx context.Context, method, path, token string, payload any, target any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return errRefused
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return errRefused
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.client.Do(req)
	if err != nil {
		return errRefused
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errRefused
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 {
		return errRefused
	}
	if target != nil && json.Unmarshal(data, target) != nil {
		return errRefused
	}
	return nil
}

func (c *controller) login(ctx context.Context, password []byte) (string, error) {
	value := strings.TrimSpace(string(password))
	if value == "" {
		return "", errRefused
	}
	var response struct {
		Token string `json:"token"`
	}
	err := c.request(ctx, "POST", "/auth/login", "", map[string]string{"username": "haloadmin", "password": value}, &response)
	if err != nil || response.Token == "" || strings.ContainsAny(response.Token, "\r\n") {
		return "", errRefused
	}
	return response.Token, nil
}

func (c *controller) identity(ctx context.Context, token, expected string) error {
	var info struct {
		ID string `json:"controller_id"`
	}
	if err := c.request(ctx, "GET", "/controller-info", token, nil, &info); err != nil {
		return err
	}
	if info.ID != expected {
		return errRefused
	}
	return nil
}

type scaleSet struct {
	ID         int64       `json:"id"`
	Repository string      `json:"repo_id"`
	Name       string      `json:"name"`
	Provider   string      `json:"provider_name"`
	Enabled    enabledFlag `json:"enabled"`
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

func (c *controller) scaleSets(ctx context.Context, token, repository string) ([]scaleSet, error) {
	var result []scaleSet
	err := c.request(ctx, "GET", "/repositories/"+repository+"/scalesets", token, nil, &result)
	if err == nil && result == nil {
		err = errRefused
	}
	return result, err
}

func (c *controller) scaleSet(ctx context.Context, token string, id int64) (scaleSet, error) {
	var result scaleSet
	err := c.request(ctx, "GET", "/scalesets/"+strconv.FormatInt(id, 10), token, nil, &result)
	return result, err
}

func (c *controller) disable(ctx context.Context, token string, id int64) error {
	return c.request(ctx, "PUT", "/scalesets/"+strconv.FormatInt(id, 10), token, map[string]bool{"enabled": false}, nil)
}
