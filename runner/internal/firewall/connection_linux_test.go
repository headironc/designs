package firewall

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdlayher/netlink"
)

// netlink.NewConn explicitly supports socket fixtures. Unlike nltest's
// synchronous callback, this fixture models a Receive interrupted by Close.
type blockedKernelSocket struct {
	entered       chan struct{}
	closed        chan struct{}
	enterOnce     sync.Once
	closeOnce     sync.Once
	closes        atomic.Int32
	deadline      time.Time
	deadlineError error
}

func (s *blockedKernelSocket) Close() error {
	s.closes.Add(1)
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}
func (s *blockedKernelSocket) Send(netlink.Message) error           { return nil }
func (s *blockedKernelSocket) SendMessages([]netlink.Message) error { return nil }
func (s *blockedKernelSocket) Receive() ([]netlink.Message, error) {
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.closed
	return nil, os.ErrClosed
}
func (s *blockedKernelSocket) SetDeadline(t time.Time) error      { s.deadline = t; return s.deadlineError }
func (s *blockedKernelSocket) SetReadDeadline(t time.Time) error  { return s.SetDeadline(t) }
func (s *blockedKernelSocket) SetWriteDeadline(t time.Time) error { return s.SetDeadline(t) }

func TestKernelCancellationInterruptsOriginalWaitAndJoinsBeforeReturn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := &blockedKernelSocket{entered: make(chan struct{}), closed: make(chan struct{})}
	conn := netlink.NewConn(s, 1)
	k, err := bindKernelSocket(ctx, cancel, nil, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	wantDeadline, _ := ctx.Deadline()
	if !s.deadline.Equal(wantDeadline) {
		t.Fatal("lost caller deadline")
	}
	returned := make(chan error, 1)
	go func() { _, err := conn.Receive(); returned <- err }()
	select {
	case <-s.entered:
	case <-time.After(time.Second):
		t.Fatal("original read did not enter")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("lost interrupted socket result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left original kernel read running")
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-k.finished:
	default:
		t.Fatal("Close returned before cancellation finished")
	}
	if s.closes.Load() != 1 {
		t.Fatal("socket closed more than once")
	}
}

func TestKernelSocketRefusesUnboundedOrFailedDeadlineAndClosesOriginal(t *testing.T) {
	failure := errors.New("deadline rejected")
	for _, bounded := range []bool{false, true} {
		ctx := context.Background()
		cancel := func() {}
		if bounded {
			ctx, cancel = context.WithTimeout(ctx, time.Second)
		}
		s := &blockedKernelSocket{entered: make(chan struct{}), closed: make(chan struct{}), deadlineError: failure}
		if _, err := bindKernelSocket(ctx, cancel, nil, netlink.NewConn(s, 1)); err == nil {
			t.Fatal("accepted missing or failed socket deadline")
		} else if bounded && !errors.Is(err, failure) {
			t.Fatal("lost deadline error cause")
		}
		if s.closes.Load() != 1 {
			t.Fatal("failed setup leaked its original socket")
		}
	}
}

func TestCanceledKernelOpenReturnsBeforeDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := openKernel(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open: %v", err)
	}
}

func TestKernelNormalCloseIsIdempotentWithoutStartingCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s := &blockedKernelSocket{entered: make(chan struct{}), closed: make(chan struct{})}
	k, err := bindKernelSocket(ctx, cancel, nil, netlink.NewConn(s, 1))
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	go func() { _ = k.Close(); _ = k.Close(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("normal repeated close waited for a stopped callback")
	}
	if s.closes.Load() != 1 {
		t.Fatal("normal close did not close exactly one original socket")
	}
}
