package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type sessionCredential struct {
	token, controller string
	store             *serviceFiles
}

func (c *sessionCredential) close() {
	if c != nil && c.store != nil {
		c.store.close()
	}
}

// Existing external credentials retain their original input contract. The new
// producer's fixed output additionally requires its trusted producer binding,
// completed original result, lifetime and a shared lease through consumer exit.
func loadGARMToken(files protectedFiles, path, parent, producer string, now time.Time) (*sessionCredential, error) {
	if path != filepath.Join(parent, "session", "garm-token") {
		if producer != "" {
			return nil, errRefused
		}
		raw, err := files.read(path, 0600, 64*1024+1)
		token := strings.TrimSpace(string(raw))
		if err != nil || !validToken(token) {
			return nil, errRefused
		}
		return &sessionCredential{token: token}, nil
	}
	if !digestPattern.MatchString(producer) || files.parents(filepath.Join(parent, "session.lock")) != nil {
		return nil, errRefused
	}
	dir, err := openPath(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, errRefused
	}
	s := &serviceFiles{files: files, parent: dir, path: filepath.Join(parent, "session"), lockPath: filepath.Join(parent, "session.lock"), raw: map[string][]byte{}, pinned: map[string]*os.File{}}
	accepted := false
	defer func() {
		if !accepted {
			s.close()
		}
	}()
	s.lock, err = openPath(s.lockPath, unix.O_RDONLY, 0)
	if err != nil || metadata(dir, files.uid, unix.S_IFDIR|0700) != nil || metadata(s.lock, files.uid, unix.S_IFREG|0600) != nil || unix.Flock(int(s.lock.Fd()), unix.LOCK_SH|unix.LOCK_NB) != nil || !s.unchanged() {
		return nil, errRefused
	}
	if exists, err := s.existing(); err != nil || !exists {
		return nil, errRefused
	}
	a := sessionAcquirer{files: files}
	if !a.absent(s, "retired.json") || !a.absent(s, "retirement.intent.json") {
		return nil, errRefused
	}
	request, err := a.read(s, "request.json", nil)
	if err != nil {
		return nil, errRefused
	}
	p, err := decodeSessionPlan(request)
	if err != nil {
		return nil, errRefused
	}
	var intent sessionIntent
	i, err := a.read(s, "intent.json", &intent)
	if err != nil || intent.Version != 1 || intent.RequestSHA != bytesSHA(request) || intent.ProducerSHA != producer || intent.Started <= 0 {
		return nil, errRefused
	}
	var seal sessionSeal
	if _, err := a.read(s, "complete.json", &seal); err != nil || a.verifyOriginal(s, i, seal) != nil || now.Unix() >= seal.Expires {
		return nil, errRefused
	}
	raw := s.raw["garm-token"]
	if len(raw) == 0 || !s.unchanged() {
		return nil, errRefused
	}
	accepted = true
	return &sessionCredential{token: strings.TrimSuffix(string(raw), "\n"), controller: p.Controller, store: s}, nil
}
