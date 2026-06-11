package wsgateway

import (
	"encoding/json"
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
	hub := NewHub(zap.NewNop(), metrics.New("ws-handler-test"))
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
