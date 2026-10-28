package lifecycle

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type sessionRetirement struct {
	Version     int    `json:"version"`
	RequestSHA  string `json:"request_sha256"`
	CompleteSHA string `json:"complete_sha256"`
	IssuerSHA   string `json:"issuer_sha256"`
	ProducerSHA string `json:"producer_sha256"`
	Expires     int64  `json:"expires_at"`
	Started     int64  `json:"started_at"`
	TokenDevice uint64 `json:"token_device"`
	TokenInode  uint64 `json:"token_inode"`
}

type sessionRejection struct {
	IntentSHA string `json:"intent_sha256"`
	BodySHA   string `json:"body_sha256"`
	Status    int    `json:"status"`
	Observed  int64  `json:"observed_at"`
}

type sessionRetirementCleanup struct {
	RejectionSHA string `json:"rejection_sha256"`
	TokenRemoved bool   `json:"consumer_token_removed"`
}

type sessionRetirer struct {
	files                    protectedFiles
	parent, issuer, producer string
	now                      func() time.Time
	fixture                  http.RoundTripper
	// The owner keeps its original controller/configuration/listener binding
	// through the observed GET. The exclusive session lease owns consumer exit.
	check func(context.Context, sessionPlan, sessionIntent, sessionWindowReady) error
}

// retirementRejection only reconstructs a retained rejection judgment. It
// neither contacts a server nor interprets file absence as token revocation.
func (a sessionAcquirer) retirementRejection(s *serviceFiles, seal sessionSeal) (sessionRetirement, error) {
	var intent sessionRetirement
	i, err := a.read(s, "retirement.intent.json", &intent)
	if err != nil || intent.Version != 1 || intent.RequestSHA != bytesSHA(s.raw["request.json"]) || intent.CompleteSHA != bytesSHA(s.raw["complete.json"]) || !digestPattern.MatchString(intent.IssuerSHA) || !digestPattern.MatchString(intent.ProducerSHA) || intent.Expires != seal.Expires || intent.Started < seal.Expires || intent.TokenDevice == 0 || intent.TokenInode == 0 {
		return intent, errRefused
	}
	var issued sessionIntent
	if strictObject(s.raw["intent.json"], &issued) != nil || issued.ProducerSHA != intent.IssuerSHA {
		return intent, errRefused
	}
	var rejection sessionRejection
	if _, err := a.read(s, "retired.json", &rejection); err != nil || rejection.IntentSHA != bytesSHA(i) || rejection.Status != 401 || rejection.Observed < intent.Started {
		return intent, errRefused
	}
	body, err := a.read(s, "retirement.rejection.body", nil)
	var wire struct {
		Error   *string `json:"error"`
		Details *string `json:"details"`
	}
	if err != nil || bytesSHA(body) != rejection.BodySHA || strictObject(body, &wire) != nil || wire.Error == nil || *wire.Error != "Authentication failed" || wire.Details == nil || *wire.Details != "" || s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil || !s.unchanged() {
		return intent, errRefused
	}
	return intent, nil
}

func (r sessionRetirer) execute(ctx context.Context, request []byte) (sessionOutcome, error) {
	out := sessionOutcome{State: "unknown"}
	p, err := decodeSessionPlan(request)
	if err != nil || !digestPattern.MatchString(r.issuer) || !digestPattern.MatchString(r.producer) || r.now == nil {
		return out, errRefused
	}
	s, err := openServiceFiles(r.files, r.parent, "session", "session.lock", false)
	if err != nil {
		return out, errRefused
	}
	defer s.close()
	if exists, err := s.existing(); err != nil || !exists {
		return out, errRefused
	}
	a := sessionAcquirer{files: r.files}
	original, err := a.read(s, "request.json", nil)
	if err != nil || !bytes.Equal(original, request) {
		return out, errRefused
	}
	var issued sessionIntent
	i, err := a.read(s, "intent.json", &issued)
	if err != nil || issued.ProducerSHA != r.issuer {
		return out, errRefused
	}
	var seal sessionSeal
	if _, err := a.read(s, "complete.json", &seal); err != nil || a.verifyOriginal(s, i, seal) != nil {
		return out, errRefused
	}
	var intent sessionRetirement
	if a.absent(s, "retired.json") {
		if ctx.Err() != nil || r.now().Unix() < seal.Expires || r.check == nil {
			return out, errRefused
		}
		window, err := s.sibling("window")
		if err != nil {
			return out, errRefused
		}
		var ready sessionWindowReady
		exists, e := window.existing()
		_, readErr := a.read(window, "window.ready.json", &ready)
		window.close()
		if e != nil || !exists || readErr != nil || r.check(ctx, p, issued, ready) != nil {
			return out, errRefused
		}
		if a.absent(s, "retirement.intent.json") {
			token := s.pinned["garm-token"]
			var st unix.Stat_t
			if token == nil || unix.Fstat(int(token.Fd()), &st) != nil || metadata(token, r.files.uid, unix.S_IFREG|0600) != nil {
				return out, errRefused
			}
			intent = sessionRetirement{1, bytesSHA(request), bytesSHA(s.raw["complete.json"]), r.issuer, r.producer, seal.Expires, r.now().Unix(), uint64(st.Dev), st.Ino}
			if s.create("retirement.intent.json", intent) != nil {
				return out, errRefused
			}
		}
		if _, err := a.read(s, "retirement.intent.json", &intent); err != nil || intent.Version != 1 || intent.TokenDevice == 0 || intent.TokenInode == 0 || intent.RequestSHA != bytesSHA(request) || intent.CompleteSHA != bytesSHA(s.raw["complete.json"]) || intent.IssuerSHA != r.issuer || intent.ProducerSHA != r.producer || intent.Expires != seal.Expires || intent.Started < seal.Expires || r.check(ctx, p, issued, ready) != nil {
			return out, errRefused
		}
		var body struct {
			Token string `json:"token"`
		}
		if strictObject(s.raw["response.body"], &body) != nil {
			return out, errRefused
		}
		client := newBoundSessionClient(nil, r.fixture, func(c context.Context) error { return r.check(c, p, issued, ready) })
		err = client.rejection(ctx, body.Token, func(raw []byte) error {
			if !a.absent(s, "retirement.rejection.body") {
				old, err := a.read(s, "retirement.rejection.body", nil)
				if err != nil || !bytes.Equal(old, raw) {
					return errRefused
				}
				return nil
			}
			return (sessionInstaller{store: s}).raw("retirement.rejection.body", raw)
		})
		client.close()
		if err != nil || ctx.Err() != nil || r.now().Unix() < seal.Expires || r.check(ctx, p, issued, ready) != nil || !s.unchanged() {
			return out, errRefused
		}
		if s.create("retired.json", sessionRejection{bytesSHA(s.raw["retirement.intent.json"]), bytesSHA(s.raw["retirement.rejection.body"]), 401, r.now().Unix()}) != nil {
			return out, errRefused
		}
	}
	intent, err = a.retirementRejection(s, seal)
	if err != nil || intent.IssuerSHA != r.issuer || intent.ProducerSHA != r.producer {
		return out, errRefused
	}
	if err := r.cleanup(s, intent); err != nil {
		return out, errRefused
	}
	return sessionOutcome{State: "expiry_rejection_observed_token_removed", Expires: seal.Expires}, nil
}

func (r sessionRetirer) cleanup(s *serviceFiles, intent sessionRetirement) error {
	a := sessionAcquirer{files: r.files}
	if !a.absent(s, "garm-token") {
		if _, err := a.read(s, "garm-token", nil); err != nil {
			return errRefused
		}
		f := s.pinned["garm-token"]
		var st unix.Stat_t
		if unix.Fstat(int(f.Fd()), &st) != nil || st.Dev != intent.TokenDevice || st.Ino != intent.TokenInode || !samePath(f, filepath.Join(s.path, "garm-token")) || !s.unchanged() {
			return errRefused
		}
		if unix.Unlinkat(int(s.dir.Fd()), "garm-token", 0) != nil {
			return errRefused
		}
		f.Close()
		delete(s.pinned, "garm-token")
		delete(s.raw, "garm-token")
	}
	if s.files.syncFile(s.dir) != nil || !a.absent(s, "garm-token") || !s.unchanged() {
		return errRefused
	}
	expected := sessionRetirementCleanup{bytesSHA(s.raw["retired.json"]), true}
	if a.absent(s, "retirement.cleanup.json") {
		if s.create("retirement.cleanup.json", expected) != nil {
			return errRefused
		}
	}
	var saved sessionRetirementCleanup
	if _, err := a.read(s, "retirement.cleanup.json", &saved); err != nil || saved != expected || s.files.syncFile(s.dir) != nil || s.files.syncFile(s.parent) != nil {
		return errRefused
	}
	return nil
}
