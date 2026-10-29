package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type observerRetirement struct {
	Request  string `json:"request_sha256"`
	Issuer   string `json:"issuer_sha256"`
	Producer string `json:"producer_sha256"`
	Started  int64  `json:"started_at"`
	Device   uint64 `json:"token_device"`
	Inode    uint64 `json:"token_inode"`
}

type observerRetirer struct {
	files                    protectedFiles
	parent, issuer, producer string
	now                      func() time.Time
	fixture                  http.RoundTripper
}

// Retirement is permitted even after the source GARM session expires or its
// controller changes. Only the original GitHub token is sent to fixed GitHub.
func (r observerRetirer) execute(ctx context.Context, raw []byte) (sessionOutcome, error) {
	p, err := decodeObserverPlan(raw)
	if err != nil || r.now == nil || !digestPattern.MatchString(r.issuer) || !digestPattern.MatchString(r.producer) {
		return sessionOutcome{}, errRefused
	}
	s, err := openServiceFiles(r.files, r.parent, "github-observer", "session.lock", false)
	if err != nil {
		return sessionOutcome{}, errRefused
	}
	defer s.close()
	if exists, err := s.existing(); err != nil || !exists {
		return sessionOutcome{}, errRefused
	}
	reader := sessionAcquirer{files: r.files}
	original, err := reader.read(s, "request.json", nil)
	if err != nil || !bytes.Equal(original, raw) {
		return sessionOutcome{}, errRefused
	}
	var signed observerIntent
	if _, err := reader.read(s, "sign.intent.json", &signed); err != nil || signed.Version != 1 || signed.Request != bytesSHA(raw) || signed.Producer != r.issuer || signed.Started <= 0 || r.now().Unix() < signed.Started {
		return sessionOutcome{}, errRefused
	}
	var intent observerRetirement
	if reader.absent(s, "retirement.intent.json") {
		intent = observerRetirement{Request: bytesSHA(raw), Issuer: r.issuer, Producer: r.producer, Started: r.now().Unix()}
		if !reader.absent(s, "github-token") {
			if _, err := reader.read(s, "github-token", nil); err != nil {
				return sessionOutcome{}, errRefused
			}
			var st unix.Stat_t
			if unix.Fstat(int(s.pinned["github-token"].Fd()), &st) != nil {
				return sessionOutcome{}, errRefused
			}
			intent.Device, intent.Inode = uint64(st.Dev), st.Ino
		}
		if s.create("retirement.intent.json", intent) != nil {
			return sessionOutcome{}, errRefused
		}
	}
	if _, err := reader.read(s, "retirement.intent.json", &intent); err != nil || intent.Request != bytesSHA(raw) || intent.Issuer != r.issuer || intent.Producer != r.producer || intent.Started < signed.Started || r.now().Unix() < intent.Started {
		return sessionOutcome{}, errRefused
	}
	base := r.fixture
	if base == nil {
		transport := observerBase()
		defer transport.CloseIdleConnections()
		base = transport
	}
	api := observerAPI{store: s, plan: p, producer: r.producer, issuer: r.issuer, base: base, now: r.now, check: func(c context.Context) error {
		if c.Err() != nil || r.now().Unix() < intent.Started || observerClock(s, r.now().Unix()) != nil {
			return errRefused
		}
		return nil
	}}
	states := map[string]string{}
	for _, name := range []string{"audit", "observer"} {
		if reader.absent(s, name+"-mint.intent.json") {
			if !reader.absent(s, name+"-mint.response.json") {
				return sessionOutcome{}, errRefused
			}
			states[name] = "not_attempted"
			continue
		}
		response, err := api.response(name + "-mint")
		if err != nil {
			return sessionOutcome{State: "unknown"}, errRefused
		}
		var wire struct {
			Token   string     `json:"token"`
			Expires *time.Time `json:"expires_at"`
		}
		if unambiguous(response.Body) != nil || json.Unmarshal(response.Body, &wire) != nil || !validToken(wire.Token) {
			return sessionOutcome{State: "unknown"}, errRefused
		}
		t := observerToken{token: wire.Token}
		if wire.Expires != nil {
			t.expires = wire.Expires.Unix()
		}
		state, err := api.retire(ctx, name, t)
		if err != nil {
			return sessionOutcome{State: "unknown"}, errRefused
		}
		states[name] = state
	}
	// The result remains tied to the retained wire facts; local receipt recovery
	// replays those facts and cannot send a second DELETE or a retained GET.
	if reader.absent(s, "retired.json") {
		if s.create("retired.json", states) != nil {
			return sessionOutcome{}, errRefused
		}
	}
	var saved map[string]string
	if _, err := reader.read(s, "retired.json", &saved); err != nil || len(saved) != 2 || saved["audit"] != states["audit"] || saved["observer"] != states["observer"] {
		return sessionOutcome{}, errRefused
	}
	if !reader.absent(s, "github-token") {
		if _, err := reader.read(s, "github-token", nil); err != nil {
			return sessionOutcome{}, errRefused
		}
		f := s.pinned["github-token"]
		var st unix.Stat_t
		if unix.Fstat(int(f.Fd()), &st) != nil || uint64(st.Dev) != intent.Device || st.Ino != intent.Inode || !samePath(f, filepath.Join(s.path, "github-token")) || !s.unchanged() || unix.Unlinkat(int(s.dir.Fd()), "github-token", 0) != nil {
			return sessionOutcome{}, errRefused
		}
		f.Close()
		delete(s.pinned, "github-token")
		delete(s.raw, "github-token")
	}
	if s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil || !s.unchanged() {
		return sessionOutcome{}, errRefused
	}
	return sessionOutcome{State: "original_tokens_retired_consumer_copy_removed"}, nil
}
