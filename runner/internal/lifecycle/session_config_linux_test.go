package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func sessionConfigurationFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	passphrase := sha256.Sum256([]byte("synthetic database passphrase for session config tests"))
	old := sha256.Sum256([]byte("synthetic old JWT signing key for session config tests"))
	entropy := sha256.Sum256([]byte("synthetic new JWT signing key for session config tests"))
	before := fmt.Sprintf(`[apiserver]
bind = "127.0.0.1"
port = 9997
use_tls = false
[database]
backend = "sqlite3"
passphrase = %q
[database.sqlite3]
db_file = %q
[jwt_auth]
secret = %q
time_to_live = "48h"
[fixture_extension]
preserve = ["one", "two"]
`, base64.RawURLEncoding.EncodeToString(passphrase[:24]), filepath.Join(t.TempDir(), "garm.db"), base64.RawURLEncoding.EncodeToString(old[:]))
	return []byte(before), entropy[:]
}

func TestSessionConfigurationChangesOnlyJWTKeyAndTTL(t *testing.T) {
	before, entropy := sessionConfigurationFixture(t)
	after, err := sessionConfiguration(before, bytes.NewReader(entropy))
	if err != nil {
		t.Fatal("native GARM candidate validation refused fixture")
	}
	var old, next map[string]any
	if _, err := toml.Decode(string(before), &old); err != nil {
		t.Fatal(err)
	}
	if _, err := toml.Decode(string(after), &next); err != nil {
		t.Fatal(err)
	}
	newAuth := next["jwt_auth"].(map[string]any)
	if newAuth["secret"] != base64.RawURLEncoding.EncodeToString(entropy) || newAuth["time_to_live"] != "24h" {
		t.Fatal("unexpected authentication-domain candidate")
	}
	next["jwt_auth"] = old["jwt_auth"]
	if !reflect.DeepEqual(old, next) || !validSessionConfiguration(before, after) {
		t.Fatal("candidate changed a setting outside the approved transition")
	}
	for _, changed := range [][]byte{
		bytes.Replace(after, []byte("port = 9997"), []byte("port = 9998"), 1),
		bytes.Replace(after, []byte("24h"), []byte("48h"), 1),
		bytes.Replace(after, []byte("preserve = [\"one\", \"two\"]"), []byte("preserve = [\"three\"]"), 1),
	} {
		if bytes.Equal(changed, after) {
			t.Fatal("fixture mutation missed serialized configuration")
		}
		if validSessionConfiguration(before, changed) {
			t.Fatal("configuration drift was accepted")
		}
	}
	if validSessionConfiguration(before, before) {
		t.Fatal("unchanged old key accepted as a new authentication domain")
	}
}

func TestSessionConfigurationFailureDoesNotReturnSecretMaterial(t *testing.T) {
	before, entropy := sessionConfigurationFixture(t)
	for _, tc := range []struct {
		body []byte
		key  []byte
	}{
		{before, entropy[:3]},
		{[]byte("invalid TOML syntax"), entropy},
		{[]byte(strings.Repeat("x", 64*1024+1)), entropy},
		{bytes.Replace(before, []byte("48h"), []byte("invalid-duration"), 1), entropy},
	} {
		out, err := sessionConfiguration(tc.body, bytes.NewReader(tc.key))
		if err == nil || out != nil || err != errRefused {
			t.Fatal("invalid configuration returned bytes or an unsafe error")
		}
	}
}

func TestSessionNativeValidationDoesNotCreateDatabase(t *testing.T) {
	before, entropy := sessionConfigurationFixture(t)
	var cfg struct {
		Database struct {
			SQLite struct {
				File string `toml:"db_file"`
			} `toml:"sqlite3"`
		} `toml:"database"`
	}
	if _, err := toml.Decode(string(before), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionConfiguration(before, bytes.NewReader(entropy)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Database.SQLite.File); !os.IsNotExist(err) {
		t.Fatal("configuration validation created or opened a runtime database")
	}
}
