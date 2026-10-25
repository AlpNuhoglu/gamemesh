package presence

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHTTPNotifierReusesConnections: the notifier is the WebSocket service's
// client for every connect, disconnect and heartbeat. With Go's default
// transport (2 idle connections per host) and an unread response body, it
// dialled a new connection for most calls under concurrency, the pattern
// that exhausted the gateway's local ports (finding #96).
func TestHTTPNotifierReusesConnections(t *testing.T) {
	var dialled atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond) // keeps the callers overlapping
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"online"}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialled.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	n := NewHTTPNotifier(srv.URL, time.Second)
	const callers, perCaller = 20, 25
	var wg sync.WaitGroup
	var failed atomic.Int64
	for range callers {
		wg.Go(func() {
			for range perCaller {
				if err := n.Connect(context.Background(), "p1"); err != nil {
					failed.Add(1)
				}
			}
		})
	}
	wg.Wait()

	require.Zero(t, failed.Load())
	t.Logf("%d calls from %d callers: %d connections", callers*perCaller, callers, dialled.Load())
	assert.LessOrEqual(t, dialled.Load(), int64(2*callers))
}
