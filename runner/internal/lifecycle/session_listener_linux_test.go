package lifecycle

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const sessionTCPHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestSessionListeningSocketRejectsWrongOrAmbiguousOwner(t *testing.T) {
	row := " 0: 0100007F:270D 00000000:0000 0A 00000000:00000000 00:00000000 00000000 24567 0 987654 1\n"
	for name, candidate := range map[string]string{
		"valid":           row,
		"wildcard":        strings.Replace(row, "0100007F", "00000000", 1),
		"foreign_uid":     strings.Replace(row, "24567", "24568", 1),
		"missing_inode":   strings.Replace(row, "987654", "0", 1),
		"wrong_port":      strings.Replace(row, "270D", "270E", 1),
		"connected":       strings.Replace(row, " 0A ", " 01 ", 1),
		"duplicate":       row + row,
		"missing_columns": " 0: 0100007F:270D 00000000:0000 0A\n",
	} {
		t.Run(name, func(t *testing.T) {
			inode, err := sessionListeningSocket([]byte(sessionTCPHeader+candidate), []byte(sessionTCPHeader), 9997, 24567)
			if name == "valid" {
				if err != nil || inode != 987654 {
					t.Fatalf("valid socket refused: %d %v", inode, err)
				}
			} else if err == nil {
				t.Fatal("unbound listener accepted")
			}
		})
	}
	if _, err := sessionListeningSocket([]byte(sessionTCPHeader+row), []byte(sessionTCPHeader+strings.Replace(row, "0100007F", strings.Repeat("0", 32), 1)), 9997, 24567); err == nil {
		t.Fatal("IPv6 listener ambiguity accepted")
	}
}

func ownSessionProcess(t *testing.T, pid int) serviceProcess {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	start, _, parsed := statIdentity(raw, int64(pid))
	if err != nil || parsed != nil {
		t.Fatal("private process identity unavailable")
	}
	return serviceProcess{PID: uint32(pid), Start: start, UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
}

func TestSessionListenerBindsActualPrivateSocketToOriginalProcess(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	h := gatewayHost{proc: "/proc", realFS: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	self := ownSessionProcess(t, os.Getpid())
	first, err := h.listener(ctx, self, port)
	if err != nil || !first.valid() {
		t.Fatalf("owned private listener rejected: %+v %v", first, err)
	}
	again, err := h.listener(ctx, self, port)
	if err != nil || again != first {
		t.Fatal("same socket lost its original identity")
	}
	child := exec.CommandContext(ctx, "/bin/sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	if _, err := h.listener(ctx, ownSessionProcess(t, child.Process.Pid), port); err == nil {
		t.Fatal("another process claimed the listening socket")
	}
	listener.Close()
	if _, err := h.listener(ctx, self, port); err == nil {
		t.Fatal("live PID without its original listener supplied server identity")
	}
}
