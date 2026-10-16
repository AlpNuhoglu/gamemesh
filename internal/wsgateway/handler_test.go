package wsgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// fakeSessions is an in-memory SessionChecker: every session exists unless
// revoked, and err, when set, makes every lookup fail.
type fakeSessions struct {
	mu      sync.Mutex
	revoked map[string]bool
	err     error
}

func (f *fakeSessions) Exists(_ context.Context, jti string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return !f.revoked[jti], nil
}

func (f *fakeSessions) ExistsMany(ctx context.Context, jtis []string) ([]bool, error) {
	out := make([]bool, len(jtis))
	for i, jti := range jtis {
		ok, err := f.Exists(ctx, jti)
		if err != nil {
			return nil, err
		}
		out[i] = ok
	}
	return out, nil
}

func (f *fakeSessions) revoke(jti string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[jti] = true
}

func newWSServer(t *testing.T) (*httptest.Server, *auth.TokenManager, *Hub) {
	t.Helper()
	srv, tokens, hub, _ := newWSServerWithSessions(t)
	return srv, tokens, hub
}

func newWSServerWithSessions(t *testing.T) (*httptest.Server, *auth.TokenManager, *Hub, *fakeSessions) {
	t.Helper()
	tokens := auth.NewTokenManager("ws-test-secret", time.Hour, "gamemesh")
	hub := NewHub(zap.NewNop(), metrics.New("ws-handler-test"), nil)
	sessions := &fakeSessions{revoked: map[string]bool{}}
	handler := NewHandler(hub, tokens, sessions, []string{"*"}, zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	handler.RegisterRoutes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, tokens, hub, sessions
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

// TestServeWSRejectsRevokedSession: a token whose session was deleted
// (logout) or never existed (a forged JTI) cannot open a connection, even
// though its signature and expiry are valid.
func TestServeWSRejectsRevokedSession(t *testing.T) {
	srv, tokens, _, sessions := newWSServerWithSessions(t)
	token, jti, err := tokens.Generate(uuid.New(), "alice")
	require.NoError(t, err)
	sessions.revoke(jti)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv.URL)+"/ws?token="+token, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestServeWSFailsClosedWhenSessionCheckFails: if the session store cannot
// answer, the handshake is refused (503) rather than let a possibly revoked
// token through.
func TestServeWSFailsClosedWhenSessionCheckFails(t *testing.T) {
	srv, tokens, _, sessions := newWSServerWithSessions(t)
	sessions.err = errors.New("redis: connection refused")
	token, _, err := tokens.Generate(uuid.New(), "alice")
	require.NoError(t, err)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv.URL)+"/ws?token="+token, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// readClose waits for the server's close frame on conn.
func readClose(t *testing.T, conn *websocket.Conn) *websocket.CloseError {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			require.ErrorAs(t, err, &ce)
			return ce
		}
	}
}

func dialToken(t *testing.T, srv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv.URL)+"/ws?token="+token, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestSweepClosesRevokedSessions: a connection whose session was deleted
// (logout) is closed with 1008 "session revoked"; others stay open.
func TestSweepClosesRevokedSessions(t *testing.T) {
	srv, tokens, hub, sessions := newWSServerWithSessions(t)
	revokedToken, revokedJTI, err := tokens.Generate(uuid.New(), "alice")
	require.NoError(t, err)
	liveToken, _, err := tokens.Generate(uuid.New(), "bob")
	require.NoError(t, err)
	revoked := dialToken(t, srv, revokedToken)
	dialToken(t, srv, liveToken)
	require.Eventually(t, func() bool { return hub.count() == 2 }, time.Second, 10*time.Millisecond)

	sessions.revoke(revokedJTI)
	hub.sweepSessions(context.Background(), sessions)

	ce := readClose(t, revoked)
	assert.Equal(t, websocket.ClosePolicyViolation, ce.Code)
	assert.Equal(t, "session revoked", ce.Text)
	assert.Eventually(t, func() bool { return hub.count() == 1 }, 2*time.Second, 10*time.Millisecond,
		"only the revoked connection is gone")
}

// TestSweepClosesExpiredTokens: an open connection must not outlive its JWT.
func TestSweepClosesExpiredTokens(t *testing.T) {
	srv, _, hub, sessions := newWSServerWithSessions(t)
	shortLived := auth.NewTokenManager("ws-test-secret", 2*time.Second, "gamemesh")
	token, _, err := shortLived.Generate(uuid.New(), "carol")
	require.NoError(t, err)
	conn := dialToken(t, srv, token)
	require.Eventually(t, func() bool { return hub.count() == 1 }, time.Second, 10*time.Millisecond)

	time.Sleep(2500 * time.Millisecond)
	hub.sweepSessions(context.Background(), sessions)

	ce := readClose(t, conn)
	assert.Equal(t, websocket.ClosePolicyViolation, ce.Code)
	assert.Equal(t, "token expired", ce.Text)
}

// TestSweepKeepsConnectionsWhenRedisFails: unlike the handshake, the sweep
// fails open, so a Redis blip does not disconnect everyone at once.
func TestSweepKeepsConnectionsWhenRedisFails(t *testing.T) {
	srv, tokens, hub, sessions := newWSServerWithSessions(t)
	token, _, err := tokens.Generate(uuid.New(), "dave")
	require.NoError(t, err)
	dialToken(t, srv, token)
	require.Eventually(t, func() bool { return hub.count() == 1 }, time.Second, 10*time.Millisecond)

	sessions.mu.Lock()
	sessions.err = errors.New("redis: connection refused")
	sessions.mu.Unlock()
	hub.sweepSessions(context.Background(), sessions)

	time.Sleep(1500 * time.Millisecond) // past closeWith's grace period
	assert.Equal(t, 1, hub.count())
}

// TestBridgeClosesRevokedSession: the logout event closes the connection
// opened with that session at once, and only that one.
func TestBridgeClosesRevokedSession(t *testing.T) {
	srv, tokens, hub, _ := newWSServerWithSessions(t)
	revokedToken, revokedJTI, err := tokens.Generate(uuid.New(), "alice")
	require.NoError(t, err)
	otherToken, _, err := tokens.Generate(uuid.New(), "alice") // same player, another device
	require.NoError(t, err)
	revoked := dialToken(t, srv, revokedToken)
	dialToken(t, srv, otherToken)
	require.Eventually(t, func() bool { return hub.count() == 2 }, time.Second, 10*time.Millisecond)

	e, err := events.New(events.TypeSessionRevoked, events.SessionRevokedPayload{JTI: revokedJTI})
	require.NoError(t, err)
	require.NoError(t, NewBridge(hub, nil, zap.NewNop()).dispatch(context.Background(), e))

	ce := readClose(t, revoked)
	assert.Equal(t, websocket.ClosePolicyViolation, ce.Code)
	assert.Equal(t, "session revoked", ce.Text)
	assert.Eventually(t, func() bool { return hub.count() == 1 }, 2*time.Second, 10*time.Millisecond,
		"the player's other session stays connected")
}
