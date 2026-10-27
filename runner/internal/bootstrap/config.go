// Package bootstrap installs the instance-scoped identity of a prebuilt runner.
package bootstrap

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	metadataEndpoint = "https://10.203.64.1:9443/api/v1/metadata"
	callbackEndpoint = "https://10.203.64.1:9443/api/v1/callbacks"
	responseLimit    = 1024 * 1024
)

// Config can only be constructed by validating the provider's rendered input.
type Config struct {
	token string
}

// Decode reads the private stdin input produced by runner-install.tmpl.
// Errors deliberately exclude input values, which include the instance token.
func Decode(input io.Reader) (Config, error) {
	var wire struct {
		MetadataURL   string `json:"metadata_url"`
		CallbackURL   string `json:"callback_url"`
		CallbackToken string `json:"callback_token"`
		UseJIT        bool   `json:"use_jit"`
	}
	raw, err := io.ReadAll(io.LimitReader(input, responseLimit+1))
	if err != nil || len(raw) > responseLimit {
		return Config{}, errors.New("invalid bootstrap input")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Config{}, errors.New("invalid bootstrap input")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, errors.New("invalid bootstrap input")
	}
	if !wire.UseJIT || strings.TrimRight(wire.MetadataURL, "/") != metadataEndpoint || strings.TrimRight(wire.CallbackURL, "/") != callbackEndpoint || wire.CallbackToken == "" {
		return Config{}, errors.New("invalid bootstrap configuration")
	}
	for _, ch := range wire.CallbackToken {
		if ch < 33 || ch > 126 {
			return Config{}, errors.New("invalid bootstrap configuration")
		}
	}
	return Config{token: wire.CallbackToken}, nil
}
