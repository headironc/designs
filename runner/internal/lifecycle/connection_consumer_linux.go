package lifecycle

import (
	"os"
	"path/filepath"
	"time"

	"github.com/headironc/designs/runner/internal/provider"
	"golang.org/x/sys/unix"
)

type connectionLease struct {
	binding connectionBinding
	plan    connectionPlan
	store   *serviceFiles
}

func (c *connectionLease) close() {
	if c != nil && c.store != nil {
		c.store.close()
	}
}

func (c *connectionLease) matches(r request, b provider.ExecutionBinding) bool {
	return c != nil && c.store.unchanged() && r.RepositoryID == 1432619931 && r.Owner == "headironc" && r.Repository == "nucleus" && r.Controller == c.binding.Controller && r.GARMRepository == c.binding.Repository && r.ScaleSet == uint32(c.binding.ScaleSet) && b.Controller == c.binding.Controller && b.Repository == c.binding.Repository && b.ScaleSet == c.plan.Scale.Name && b.Pool == c.plan.Scale.Name+"-"+c.binding.Repository && b.Image == c.plan.ImageSHA
}

func loadConnection(files protectedFiles, parent, producer string, now time.Time) (*connectionLease, error) {
	if !digestPattern.MatchString(producer) || files.parents(filepath.Join(parent, "session.lock")) != nil {
		return nil, errRefused
	}
	dir, err := openPath(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, errRefused
	}
	s := &serviceFiles{files: files, parent: dir, path: filepath.Join(parent, "connection"), lockPath: filepath.Join(parent, "session.lock"), raw: map[string][]byte{}, pinned: map[string]*os.File{}}
	accepted := false
	defer func() {
		if !accepted {
			s.close()
		}
	}()
	if observerSharedLease(s) != nil {
		return nil, errRefused
	}
	if exists, err := s.existing(); err != nil || !exists {
		return nil, errRefused
	}
	reader := sessionAcquirer{files: files}
	if !reader.absent(s, "controller.stop.intent.json") {
		return nil, errRefused
	}
	raw, err := reader.read(s, "request.json", nil)
	if err != nil {
		return nil, errRefused
	}
	p, err := decodeConnectionPlan(raw)
	if err != nil {
		return nil, errRefused
	}
	binding, err := validateConnection(s, p, producer, now.Unix())
	if err != nil {
		return nil, errRefused
	}
	var derived connectionBinding
	if _, err := reader.read(s, "connection.json", &derived); err != nil || derived != binding {
		return nil, errRefused
	}
	var complete struct {
		Verified string `json:"verified_sha256"`
	}
	if _, err := reader.read(s, "complete.json", &complete); err != nil || complete.Verified != bytesSHA(s.raw["verified.json"]) || !s.unchanged() {
		return nil, errRefused
	}
	accepted = true
	return &connectionLease{binding, p, s}, nil
}
