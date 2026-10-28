//go:build linux

// Own the module test's descendants without changing its PID or /proc view.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type receipt struct {
	pid      int
	exitCode *int
	signal   *int
	closed   bool
	failures []string
}

func main() {
	output := os.NewFile(3, "supervisor-receipt")
	if output == nil {
		os.Exit(1)
	}
	r := supervise(os.Args[1:])
	// A fixed-length record rejects duplicate-key ambiguity at the consumer.
	// Fields: version, target PID, exit code, signal, cleanup complete, failures.
	err := json.NewEncoder(output).Encode([6]any{1, r.pid, r.exitCode, r.signal, r.closed, r.failures})
	closeErr := output.Close()
	if err != nil || closeErr != nil || !r.closed || len(r.failures) != 0 || r.exitCode == nil || *r.exitCode != 0 {
		os.Exit(1)
	}
}

func supervise(args []string) receipt {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	r := receipt{failures: []string{}}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(interrupts)
	const childSubreaper = 36
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, childSubreaper, 1, 0, 0, 0, 0)
	if errno != 0 {
		r.failures = append(r.failures, "enable child subreaper: "+errno.Error())
	} else if len(args) == 0 {
		r.failures = append(r.failures, "target command is missing")
	} else {
		command, err := exec.LookPath(args[0])
		if err == nil {
			var child *os.Process
			// The private receipt descriptor is intentionally not inherited.
			child, err = os.StartProcess(command, args, &os.ProcAttr{Env: os.Environ(), Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
			if err == nil {
				r.pid = child.Pid
				// Only the wait4 loop below may reap children.
				_ = child.Release()
			}
		}
		if err != nil {
			r.failures = append(r.failures, "start target: "+err.Error())
		}
	}
	var stopping time.Time
	for {
		select {
		case received := <-interrupts:
			r.failures = append(r.failures, "supervisor received "+received.String())
			if stopping.IsZero() {
				stopping = time.Now()
			}
		default:
		}
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.ECHILD) {
				r.closed = true
				return r
			}
			if err != nil {
				r.failures = append(r.failures, "reap child: "+err.Error())
				return r
			}
			if pid == 0 {
				break
			}
			if pid == r.pid {
				if status.Exited() {
					code := status.ExitStatus()
					r.exitCode = &code
				} else if status.Signaled() {
					value := int(status.Signal())
					r.signal = &value
				}
			}
		}
		if (r.exitCode != nil || r.signal != nil) && stopping.IsZero() {
			r.failures = append(r.failures, "target exited with surviving descendants")
			stopping = time.Now()
		}
		if !stopping.IsZero() {
			if time.Since(stopping) >= 8*time.Second {
				r.failures = append(r.failures, "descendant cleanup deadline exceeded")
				return r
			}
			children, err := directChildren()
			if err != nil {
				r.failures = append(r.failures, "read owned children: "+err.Error())
				return r
			}
			value := syscall.SIGTERM
			if time.Since(stopping) >= 2*time.Second {
				value = syscall.SIGKILL
			}
			// No wait/reaper runs between ownership discovery and signalling.
			// An exited child remains unreaped, so its PID cannot be reused here.
			for _, pid := range children {
				if err := syscall.Kill(pid, value); err != nil && !errors.Is(err, syscall.ESRCH) {
					r.failures = append(r.failures, fmt.Sprintf("signal owned child %d: %v", pid, err))
					return r
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func directChildren() ([]int, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, err
	}
	var children []int
	for _, task := range tasks {
		raw, err := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "children"))
		if err != nil {
			return nil, err
		}
		for _, field := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 {
				return nil, fmt.Errorf("invalid child PID %q", field)
			}
			children = append(children, pid)
		}
	}
	return children, nil
}
