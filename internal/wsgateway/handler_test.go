package wsgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

func newWSServer(t *testing.T) (*httptest.Server, *auth.TokenManager, *Hub) {
	t.Helper()
	tokens := auth.NewTokenManager("ws-test-secret", time.Hour, "gamemesh")
	hub := NewHub(zap.NewNop(), metrics.New("ws-handler-test"), nil)
	handler := NewHandler(hub, tokens, []string{"*"}, zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	handler.RegisterRoutes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, tokens, hub
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestServeWSRejectsMissingOrBadToken(t *testing.T) {
	srv, _, _ := newWSServer(t)

	resp, err := http.Get(srv.URL + "/ws")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, err = http.Get(srv.URL + "/ws?token=garbage")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWebSocketConnectJoinAndReceive(t *testing.T) {
	srv, tokens, _ := newWSServer(t)

	token, _, err := tokens.Generate(uuid.New(), "alice")
	require.NoError(t, err)

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv.URL)+"/ws?token="+token, nil)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	// Join a room; the joiner itself receives the PlayerJoined broadcast.
	require.NoError(t, conn.WriteJSON(map[string]string{"action": "join", "room": "room-1"}))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err)

	var msg Message
	require.NoError(t, json.Unmarshal(raw, &msg))
	assert.Equal(t, "PlayerJoined", msg.Type)
	assert.Equal(t, "room-1", msg.Room)
}

func TestWebSocketAuthorizationHeaderFallback(t *testing.T) {
	srv, tokens, _ := newWSServer(t)

	token, _, err := tokens.Generate(uuid.New(), "bob")
	require.NoError(t, err)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv.URL)+"/ws", header)
	require.NoError(t, err)
	conn.Close()
}

// dialClients connects one client per name and waits until the hub has them.
func dialClients(t *testing.T, srv *httptest.Server, tokens *auth.TokenManager, hub *Hub, names ...string) []*websocket.Conn {
	t.Helper()
	conns := make([]*websocket.Conn, 0, len(names))
	for _, name := range names {
		token, _, err := tokens.Generate(uuid.New(), name)
		require.NoError(t, err)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv.URL)+"/ws?token="+token, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		conns = append(conns, conn)
	}
	require.Eventually(t, func() bool { return hub.count() == len(names) }, time.Second, 10*time.Millisecond)
	return conns
}

// TestCloseAllSendsGoingAway: on shutdown every client gets close code 1001
// with a reason instead of an abrupt 1006, and the hub empties as soon as
// they answer.
func TestCloseAllSendsGoingAway(t *testing.T) {
	srv, tokens, hub := newWSServer(t)
	conns := dialClients(t, srv, tokens, hub, "alice", "bob")

	closes := make(chan *websocket.CloseError, len(conns))
	for _, conn := range conns {
		go func() { // a reading client sees the close frame and answers it
			_, _, err := conn.ReadMessage()
			var ce *websocket.CloseError
			errors.As(err, &ce)
			closes <- ce
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	hub.CloseAll(ctx)
	assert.Less(t, time.Since(start), time.Second, "no need to wait for the deadline")
	assert.Zero(t, hub.count())
	for range conns {
		ce := <-closes
		require.NotNil(t, ce)
		assert.Equal(t, websocket.CloseGoingAway, ce.Code)
		assert.Equal(t, "server shutting down", ce.Text)
	}
}

// TestCloseAllClosesUnresponsiveClients: a client that never answers the
// close frame is cut off when the shutdown budget runs out.
func TestCloseAllClosesUnresponsiveClients(t *testing.T) {
	srv, tokens, hub := newWSServer(t)
	dialClients(t, srv, tokens, hub, "carol") // never reads, so never answers

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	hub.CloseAll(ctx)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 200*time.Millisecond)
	assert.Less(t, elapsed, time.Second)
	assert.Eventually(t, func() bool { return hub.count() == 0 }, time.Second, 10*time.Millisecond)
}
