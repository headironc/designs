package lifecycle

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io"
	"reflect"
	"time"

	"github.com/BurntSushi/toml"
	garmconfig "github.com/cloudbase/garm/config"
)

// Configuration rewriting belongs to this authentication-domain transition.
// GARM's own validator checks the complete candidate; the map comparison keeps
// fields unknown to this consumer rather than dropping or defaulting them.
func sessionConfiguration(before []byte, entropy io.Reader) ([]byte, error) {
	if len(before) > 64*1024 || entropy == nil {
		return nil, errRefused
	}
	var original, candidate map[string]any
	if _, err := toml.Decode(string(before), &original); err != nil {
		return nil, errRefused
	}
	if _, err := toml.Decode(string(before), &candidate); err != nil {
		return nil, errRefused
	}
	oldAuth, ok := original["jwt_auth"].(map[string]any)
	newAuth, ko := candidate["jwt_auth"].(map[string]any)
	if !ok || !ko {
		return nil, errRefused
	}
	oldSecret, so := oldAuth["secret"].(string)
	oldTTL, to := oldAuth["time_to_live"].(string)
	if !so || oldSecret == "" || !to {
		return nil, errRefused
	}
	if _, err := time.ParseDuration(oldTTL); err != nil {
		return nil, errRefused
	}
	var key [32]byte
	if _, err := io.ReadFull(entropy, key[:]); err != nil {
		return nil, errRefused
	}
	secret := base64.RawURLEncoding.EncodeToString(key[:])
	if secret == oldSecret {
		return nil, errRefused
	}
	newAuth["secret"], newAuth["time_to_live"] = secret, "24h"
	var encoded bytes.Buffer
	if toml.NewEncoder(&encoded).Encode(candidate) != nil || encoded.Len() > 64*1024 {
		return nil, errRefused
	}
	if !validSessionConfiguration(before, encoded.Bytes()) {
		return nil, errRefused
	}
	return encoded.Bytes(), nil
}

func validSessionConfiguration(before, after []byte) bool {
	if len(before) > 64*1024 || len(after) > 64*1024 {
		return false
	}
	var original, candidate map[string]any
	if _, err := toml.Decode(string(before), &original); err != nil {
		return false
	}
	if _, err := toml.Decode(string(after), &candidate); err != nil {
		return false
	}
	oldAuth, ok := original["jwt_auth"].(map[string]any)
	newAuth, ko := candidate["jwt_auth"].(map[string]any)
	if !ok || !ko {
		return false
	}
	oldSecret, so := oldAuth["secret"].(string)
	secret, sn := newAuth["secret"].(string)
	oldTTL, to := oldAuth["time_to_live"].(string)
	if !so || !sn || oldSecret == "" || oldSecret == secret || !to || newAuth["time_to_live"] != "24h" {
		return false
	}
	// Compare the complete semantic configuration before GARM's validator can
	// read any configured files. Recovery validates the retained candidate,
	// never generates another key to replace an interrupted transition.
	newAuth["secret"], newAuth["time_to_live"] = oldSecret, oldTTL
	if !reflect.DeepEqual(candidate, original) {
		return false
	}
	var native garmconfig.Config
	_, err := toml.Decode(string(after), &native)
	// This controller uses database-managed forge credentials and the fixed
	// loopback HTTP endpoint. Reject unsupported file-key/TLS sources before
	// the native validator could open those additional secrets.
	return err == nil && native.APIServer.Bind == "127.0.0.1" && native.APIServer.Port == 9997 && !native.APIServer.UseTLS && len(native.Github) == 0 && native.Database.DbBackend == garmconfig.SQLiteBackend && native.JWTAuth.TimeToLive.Duration() == 24*time.Hour && native.Validate() == nil
}

func prepareSessionConfiguration(before []byte) ([]byte, error) {
	return sessionConfiguration(before, rand.Reader)
}
