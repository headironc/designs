// Package firewall owns the dedicated service's validated atomic replacement.
package firewall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type configurationFile struct {
	parent, file *os.File
	path         string
	uid          uint32
	raw          []byte
}

func openConfiguration(path string, uid uint32) (*configurationFile, error) {
	parent, err := protectedPath(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	f, err := protectedAt(parent, filepath.Base(path), unix.O_RDONLY, 0)
	if err != nil {
		_ = parent.Close()
		return nil, err
	}
	c := &configurationFile{parent: parent, file: f, path: path, uid: uid}
	fail := func(err error) (*configurationFile, error) { c.Close(); return nil, err }
	c.raw, err = protectedBytes(f, uid, 16*1024)
	if err != nil {
		return fail(err)
	}
	if _, err := ParseConfiguration(c.raw); err != nil {
		return fail(err)
	}
	if !c.unchanged() {
		return fail(errState)
	}
	return c, nil
}

func (c *configurationFile) Close() { _ = c.file.Close(); _ = c.parent.Close() }

func (c *configurationFile) unchanged() bool {
	var st unix.Stat_t
	if unix.Fstat(int(c.parent.Fd()), &st) != nil || st.Uid != c.uid || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0022 != 0 {
		return false
	}
	parent, err := protectedPath(filepath.Dir(c.path), unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return false
	}
	defer parent.Close()
	if !sameOpenFile(c.parent, parent) {
		return false
	}
	f, err := protectedAt(c.parent, filepath.Base(c.path), unix.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	raw, err := protectedBytes(f, c.uid, 16*1024)
	return err == nil && sameOpenFile(c.file, f) && bytes.Equal(raw, c.raw)
}

func currentKernelBinding(producer string) (kernelBinding, error) {
	f, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return kernelBinding{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, 65))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return kernelBinding{}, err
	}
	var namespace unix.Stat_t
	if unix.Stat("/proc/self/ns/net", &namespace) != nil {
		return kernelBinding{}, errState
	}
	binding := kernelBinding{Producer: producer, Boot: strings.TrimSuffix(string(raw), "\n"), Device: uint64(namespace.Dev), Inode: namespace.Ino}
	if !binding.valid() {
		return kernelBinding{}, errState
	}
	return binding, nil
}

func executableStat(f *os.File) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return st, err
	}
	if st.Uid != 0 || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 || st.Nlink != 1 || st.Size <= 0 || st.Size > 256*1024*1024 {
		return st, errState
	}
	return st, nil
}

// Reload consumes entrypoint-owned fixed paths. The runtime JSON is not nft
// text; there is no subprocess or CLI fallback. Existing unknown operations
// are only observed, never transmitted again.
func Reload(ctx context.Context, configurationPath, statePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("dedicated firewall reload requires the root service identity")
	}
	c, err := openConfiguration(configurationPath, 0)
	if err != nil {
		return fmt.Errorf("load protected firewall configuration: %w", err)
	}
	defer c.Close()
	// This fixed proc link identifies the running image, including when the
	// installed pathname changes. No caller-selected executable is opened.
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer executable.Close()
	initial, err := executableStat(executable)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(executable, 256*1024*1024+1))
	if err != nil || n != initial.Size {
		return errors.Join(errState, err)
	}
	producer := hex.EncodeToString(hash.Sum(nil))
	binding, err := currentKernelBinding(producer)
	if err != nil {
		return err
	}
	admit := func() error {
		current, err := executableStat(executable)
		if err != nil || current.Dev != initial.Dev || current.Ino != initial.Ino || current.Size != initial.Size || current.Mtim != initial.Mtim || current.Ctim != initial.Ctim || current.Mode != initial.Mode || current.Gid != initial.Gid || !c.unchanged() {
			return errState
		}
		observed, err := currentKernelBinding(producer)
		if err != nil {
			return err
		}
		if observed != binding {
			return errState
		}
		return nil
	}
	if err := admit(); err != nil {
		return err
	}
	s, err := openFirewallFiles(statePath, 0)
	if err != nil {
		return fmt.Errorf("lock protected firewall operation: %w", err)
	}
	defer s.Close()
	return reloadTransaction(ctx, c.raw, binding, s, func(ctx context.Context) (transactionKernel, error) { return openKernel(ctx) }, admit)
}
