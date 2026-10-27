package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type boundedOutput struct{ buffer bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > 2*1024*1024-b.buffer.Len() {
		return 0, errRefused
	}
	return b.buffer.Write(p)
}

func pinnedProgram(path string, uid, mode uint32, expected string) (*os.File, error) {
	f, err := openPath(path, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	if metadata(f, uid, unix.S_IFREG|mode) != nil {
		f.Close()
		return nil, errRefused
	}
	h := sha256.New()
	before, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	_, copyErr := io.Copy(h, f)
	after, statErr := f.Stat()
	if copyErr != nil || statErr != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || fmt.Sprintf("%x", h.Sum(nil)) != expected || !samePath(f, path) {
		f.Close()
		return nil, errRefused
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func invokeCreate(ctx context.Context, g *guardFiles, p createPolicy, input []byte) ([]byte, error) {
	return invokeNative(ctx, g, p.ProviderSHA, p.ConfigSHA, []string{"GARM_COMMAND=CreateInstance", "GARM_CONTROLLER_ID=" + p.Controller, "GARM_POOL_ID=" + p.Pool}, input)
}

func invokeNative(ctx context.Context, g *guardFiles, providerSHA, configSHA string, environment []string, input []byte) ([]byte, error) {
	native, err := pinnedProgram(g.s.native, g.s.rootUID, 0755, providerSHA)
	if err != nil {
		return nil, err
	}
	defer native.Close()
	config, err := pinnedProgram(g.s.config, g.s.rootUID, 0644, configSHA)
	if err != nil {
		return nil, err
	}
	defer config.Close()
	if ctx.Err() != nil || !g.unchanged() || absent(g.s.maintenance) != nil {
		return nil, errRefused
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3")
	// The inherited lock keeps a killed guard's still-running native process
	// serialized. The intent independently prevents a new invocation after exit.
	cmd.ExtraFiles = []*os.File{native, config, g.lock}
	cmd.Env = append(environment, "GARM_INTERFACE_VERSION=v0.1.0", "GARM_PROVIDER_CONFIG_FILE=/proc/self/fd/4")
	cmd.Stdin = bytes.NewReader(input)
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return unix.Kill(-cmd.Process.Pid, unix.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	// A successful leader is insufficient if a descendant remains in its group.
	if ctx.Err() != nil || unix.Kill(-cmd.Process.Pid, 0) != unix.ESRCH {
		return nil, errRefused
	}
	return output.buffer.Bytes(), nil
}
