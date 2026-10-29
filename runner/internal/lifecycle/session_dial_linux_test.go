package lifecycle

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionDialVerifiesPeerBeforeCredentialBytes(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		name := "refused"
		if allowed {
			name = "accepted"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			verified := atomic.Bool{}
			type result struct {
				count int
				after bool
				err   error
			}
			done := make(chan result, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- result{err: err}
					return
				}
				defer conn.Close()
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				buf := make([]byte, 1)
				n, err := conn.Read(buf)
				done <- result{n, verified.Load(), err}
			}()
			client := newBoundSessionClient(nil, nil, func(c context.Context) error {
				if c.Err() != nil {
					return c.Err()
				}
				verified.Store(true)
				if !allowed {
					return errRefused
				}
				return nil
			})
			defer client.close()
			transport, ok := client.wire.base.(*http.Transport)
			if !ok {
				t.Fatal("production client did not install its bound transport")
			}
			conn, err := transport.DialContext(ctx, "tcp", listener.Addr().String())
			if !allowed {
				if err == nil || conn != nil {
					t.Fatal("refused peer received a usable connection")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Write([]byte("X")); err != nil {
					t.Fatal(err)
				}
				conn.Close()
			}
			select {
			case got := <-done:
				if !verified.Load() || !got.after || !allowed && got.count != 0 || allowed && got.count != 1 {
					t.Fatalf("peer verification did not precede transmission: %+v", got)
				}
			case <-ctx.Done():
				t.Fatal("private peer did not finish")
			}
		})
	}
}
