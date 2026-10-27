package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func absentAt(dir *os.File, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err == unix.ENOENT {
		return nil
	}
	return errRefused
}

// persist keeps a durable refusal marker until the replacement is durable.
// Interrupted transactions retain their files for inspection, never automatic replay.
func (g *guardFiles) persist(ctx context.Context, next []byte, syncDir func() error) error {
	if ctx.Err() != nil || !g.unchanged() || absent(g.s.maintenance) != nil || len(next) > 64*1024 {
		return errRefused
	}
	marker, _ := json.Marshal(struct {
		Before string `json:"before_sha256"`
		After  string `json:"after_sha256"`
	}{fmt.Sprintf("%x", sha256.Sum256(g.raw)), fmt.Sprintf("%x", sha256.Sum256(next))})
	if err := g.create("state.pending", marker); err != nil {
		return err
	}
	if err := syncDir(); err != nil {
		return err
	}
	if err := g.create("state.new", next); err != nil {
		return err
	}
	if ctx.Err() != nil || !g.unchanged() || absent(g.s.maintenance) != nil {
		return errRefused
	}
	fd := int(g.dir.Fd())
	if err := unix.Renameat(fd, "state.new", fd, "state.json"); err != nil {
		return err
	}
	if err := syncDir(); err != nil {
		return err
	}
	current, err := openAt(g.dir, "state.json")
	if err != nil {
		return err
	}
	data, err := readFile(current, g.s.serviceUID, 0600, 64*1024)
	if err != nil || string(data) != string(next) {
		current.Close()
		return errRefused
	}
	g.file.Close()
	g.file, g.raw = current, data
	if !g.unchanged() || ctx.Err() != nil || absent(g.s.maintenance) != nil {
		return errRefused
	}
	if err := unix.Unlinkat(fd, "state.pending", 0); err != nil {
		return err
	}
	// If this fails, the replacement is already durable; after reboot the marker
	// may reappear and refuse. A missing marker never permits an uncommitted state.
	return syncDir()
}

func (g *guardFiles) create(name string, data []byte) error {
	fd, err := unix.Openat(int(g.dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if err := metadata(f, g.s.serviceUID, unix.S_IFREG|0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
