package firewall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
)

// One local kernel transaction must finish inside this bound. Expiration
// ends the socket wait; it does not prove whether a sent batch committed.
const kernelTimeout = 30 * time.Second

type kernelSocket struct {
	*kernelReader
	context      context.Context
	cancel       context.CancelFunc
	stop         func() bool
	finished     chan struct{}
	closeOnce    sync.Once
	shutdownOnce sync.Once
	closeErr     error
}

func openKernel(ctx context.Context) (*kernelSocket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, kernelTimeout)
	var socket *netlink.Conn
	conn, err := nftables.New(nftables.AsLasting(), nftables.WithSockOptions(func(c *netlink.Conn) error {
		socket = c
		return nil
	}))
	if err != nil {
		cancel()
		if socket != nil {
			_ = socket.Close()
		}
		return nil, fmt.Errorf("open firewall kernel connection: %w", err)
	}
	return bindKernelSocket(bounded, cancel, conn, socket)
}

func bindKernelSocket(ctx context.Context, cancel context.CancelFunc, conn *nftables.Conn, socket *netlink.Conn) (*kernelSocket, error) {
	deadline, ok := ctx.Deadline()
	if socket == nil || !ok {
		cancel()
		if socket != nil {
			_ = socket.Close()
		}
		return nil, errors.New("missing bounded firewall kernel socket")
	}
	if err := socket.SetDeadline(deadline); err != nil {
		cancel()
		_ = socket.Close()
		return nil, fmt.Errorf("bound firewall kernel socket: %w", err)
	}
	k := &kernelSocket{kernelReader: &kernelReader{Conn: conn, socket: socket}, context: ctx, cancel: cancel, finished: make(chan struct{})}
	// Flush holds the SDK mutex while waiting for acknowledgements.
	// CloseLasting takes that same mutex. Close the captured native socket
	// directly so cancellation can interrupt the original blocked Flush.
	k.stop = context.AfterFunc(ctx, func() {
		k.closeSocket()
		close(k.finished)
	})
	if err := ctx.Err(); err != nil {
		_ = k.Close()
		return nil, err
	}
	return k, nil
}

func (k *kernelSocket) closeSocket() {
	k.closeOnce.Do(func() { k.closeErr = k.socket.Close() })
}

// Close joins cancellation before the operation owner releases its lock.
// The SDK connection is never reused after this close, avoiding its automatic
// transient redial behavior after CloseLasting.
func (k *kernelSocket) Close() error {
	k.shutdownOnce.Do(func() {
		if !k.stop() {
			<-k.finished
		}
		k.cancel()
		k.closeSocket()
	})
	return k.closeErr
}
