//go:build unix

package wsgateway

import (
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handler.go does not set TCP_NODELAY: Go turns Nagle's algorithm off on
// every TCP socket it creates. This test reads the option off a real upgraded
// connection, so that is a checked fact rather than an assumption, and it
// fails if the default or a later change here takes it away. See
// docs/latency.md, B2.

// serverConn returns the server's side of the only connection in the hub.
func serverConn(t *testing.T, hub *Hub) *websocket.Conn {
	t.Helper()
	var conn *websocket.Conn
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		for c := range hub.clients {
			conn = c.conn
		}
		return conn != nil
	}, time.Second, 5*time.Millisecond)
	return conn
}

// noDelay reads TCP_NODELAY from the socket itself.
func noDelay(t *testing.T, conn net.Conn) bool {
	t.Helper()
	tcp, ok := conn.(*net.TCPConn)
	require.True(t, ok, "not a TCP connection: %T", conn)
	raw, err := tcp.SyscallConn()
	require.NoError(t, err)
	var value int
	var optErr error
	require.NoError(t, raw.Control(func(fd uintptr) {
		value, optErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
	}))
	require.NoError(t, optErr)
	return value != 0
}

// A relay frame is far smaller than a TCP segment. With Nagle's algorithm on,
// the kernel may hold such a frame back until the previous one is
// acknowledged, which a peer's delayed ACK can stretch to tens of
// milliseconds. TCP_NODELAY must be set on the server's socket.
func TestServerSocketHasNagleDisabled(t *testing.T) {
	srv, tokens, hub := newWSServer(t)
	token, _, err := tokens.Generate(uuid.New(), "player")
	require.NoError(t, err)
	client := dialToken(t, srv, token)

	assert.True(t, noDelay(t, serverConn(t, hub).NetConn()), "TCP_NODELAY is off on the server's socket")
	assert.True(t, noDelay(t, client.NetConn()), "TCP_NODELAY is off on the client's socket")
}
