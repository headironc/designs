package watchdog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSDKWriteCancellationDoesNotRetry(t *testing.T) {
	for _, method := range []string{"login", "disable"} {
		t.Run(method, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					close(entered)
				}
				<-release
			}))
			defer server.Close()
			defer close(release)
			api := newController(server.Listener.Addr().String())
			defer api.client.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if method == "login" {
					_, err := api.login(ctx, []byte("synthetic-password"))
					done <- err
				} else {
					done <- api.disable(ctx, "synthetic-session", 4294967295)
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("write did not arrive")
			}
			cancel()
			select {
			case err := <-done:
				if err != errRefused || requests.Load() != 1 {
					t.Fatalf("err=%v requests=%d", err, requests.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("SDK ignored request cancellation")
			}
		})
	}
}
