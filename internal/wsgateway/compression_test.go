package wsgateway

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handler.go does not set EnableCompression: gorilla negotiates no
// compression unless asked. This test offers it from the client and checks
// the handshake, so that is a checked fact, and it fails if the default or a
// later change here takes it away. See docs/latency.md, B3.

// permessage-deflate costs CPU on every frame and saves nothing on packets
// this small. The server must not agree to it even when the client offers.
func TestServerDoesNotNegotiateCompression(t *testing.T) {
	srv, tokens, _ := newWSServer(t)
	token, _, err := tokens.Generate(uuid.New(), "player")
	require.NoError(t, err)

	dialer := websocket.Dialer{EnableCompression: true} // offers permessage-deflate
	conn, resp, err := dialer.Dial(wsURL(srv.URL)+"/ws?token="+token, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	assert.Empty(t, resp.Header.Values("Sec-WebSocket-Extensions"),
		"the server agreed to a WebSocket extension")
}
