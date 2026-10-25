package gateway

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// echoUpstream records what the proxied request looked like on arrival.
type echo struct {
	Path   string `json:"path"`
	UserID string `json:"user_id"`
	ReqID  string `json:"request_id"`
	XFF    string `json:"x_forwarded_for"`
	RealIP string `json:"x_real_ip"`
	Host   string `json:"host"`
}

// newGatewayUnderTest runs the gateway engine in a real HTTP server:
// httputil.ReverseProxy needs a live ResponseWriter (httptest.ResponseRecorder
// does not implement CloseNotifier through gin).
func newGatewayUnderTest(t *testing.T, upstreamURL string) (*httptest.Server, *auth.TokenManager) {
	t.Helper()
	return newGatewayWithTrustedProxies(t, upstreamURL, nil)
}

// newGatewayWithTrustedProxies is newGatewayUnderTest with the TRUSTED_PROXIES
// setting that server.NewEngine applies in production (nil trusts none).
func newGatewayWithTrustedProxies(t *testing.T, upstreamURL string, trustedProxies []string) (*httptest.Server, *auth.TokenManager) {
	t.Helper()

	cfg := config.Load("gateway")
	cfg.PlayerServiceURL = upstreamURL
	cfg.MatchmakingServiceURL = upstreamURL
	cfg.LeaderboardServiceURL = upstreamURL
	cfg.WebsocketServiceURL = upstreamURL

	tokens := auth.NewTokenManager("gw-test-secret", time.Hour, "gamemesh")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	require.NoError(t, r.SetTrustedProxies(trustedProxies))
	r.Use(middleware.RequestID())
	RegisterRoutes(r, cfg, tokens, nil, zap.NewNop()) // nil sessions → JWT-only

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, tokens
}

func newEchoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(echo{
			Path:   r.URL.Path,
			UserID: r.Header.Get(middleware.HeaderUserID),
			ReqID:  r.Header.Get(middleware.HeaderRequestID),
			XFF:    strings.Join(r.Header.Values("X-Forwarded-For"), ", "),
			RealIP: r.Header.Get("X-Real-IP"),
			Host:   r.Host,
		})
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func decodeEcho(t *testing.T, resp *http.Response) echo {
	t.Helper()
	defer resp.Body.Close()
	var got echo
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	return got
}

func TestPublicRouteStripsPrefix(t *testing.T) {
	gw, _ := newGatewayUnderTest(t, newEchoUpstream(t).URL)

	resp, err := http.Get(gw.URL + "/api/v1/leaderboard/top/10")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := decodeEcho(t, resp)
	assert.Equal(t, "/leaderboard/top/10", got.Path, "gateway strips /api/v1")
	assert.NotEmpty(t, got.ReqID, "request ID propagates downstream")
}

func TestProtectedRouteRequiresJWT(t *testing.T) {
	gw, tokens := newGatewayUnderTest(t, newEchoUpstream(t).URL)

	// No token → 401, request never reaches the upstream.
	resp, err := http.Post(gw.URL+"/api/v1/queue", "application/json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Valid token → proxied with X-User-ID set from the JWT.
	playerID := uuid.New()
	token, _, err := tokens.Generate(playerID, "alice")
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/api/v1/queue", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := decodeEcho(t, resp)
	assert.Equal(t, "/queue", got.Path)
	assert.Equal(t, playerID.String(), got.UserID)
}

func TestSpoofedIdentityHeaderIsStripped(t *testing.T) {
	gw, tokens := newGatewayUnderTest(t, newEchoUpstream(t).URL)

	realID := uuid.New()
	token, _, err := tokens.Generate(realID, "alice")
	require.NoError(t, err)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/api/v1/queue", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(middleware.HeaderUserID, "attacker-chosen-id")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := decodeEcho(t, resp)
	assert.Equal(t, realID.String(), got.UserID,
		"identity always comes from the JWT, never the client header")
}

func TestProxyForwardsOnlyTheResolvedClientIP(t *testing.T) {
	gw, _ := newGatewayUnderTest(t, newEchoUpstream(t).URL)

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/v1/leaderboard", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Real-IP", "7.7.7.7")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := decodeEcho(t, resp)
	assert.Equal(t, "127.0.0.1", got.XFF, "upstreams see the peer address, not the client's claim")
	assert.Empty(t, got.RealIP, "a client-supplied X-Real-IP is not forwarded")
}

func TestProxyBehindTrustedProxyForwardsTheRealClient(t *testing.T) {
	// The test client connects from 127.0.0.1 and stands in for Traefik.
	gw, _ := newGatewayWithTrustedProxies(t, newEchoUpstream(t).URL, []string{"127.0.0.1"})

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/v1/leaderboard", nil)
	// A spoofed hop, then the address the trusted proxy actually saw.
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := decodeEcho(t, resp)
	assert.Equal(t, "203.0.113.9", got.XFF, "only the resolved client is forwarded, not the chain")
}

func TestProxyKeepsInboundHost(t *testing.T) {
	gw, _ := newGatewayUnderTest(t, newEchoUpstream(t).URL)

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/v1/leaderboard", nil)
	req.Host = "api.gamemesh.test"
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "api.gamemesh.test", decodeEcho(t, resp).Host)
}

func TestWebSocketUpgradeIsProxied(t *testing.T) {
	var upgrader websocket.Upgrader
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(mt, msg)
	}))
	t.Cleanup(upstream.Close)
	gw, _ := newGatewayUnderTest(t, upstream.URL)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gw.URL, "http")+"/ws", nil)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("ping")))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "ping", string(msg))
}

func TestPublicHealthShowsOnlyStatusAndVersion(t *testing.T) {
	t.Setenv("SERVICE_VERSION", "abc1234")
	gw, _ := newGatewayUnderTest(t, newEchoUpstream(t).URL)

	resp, err := http.Get(gw.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"ok","version":"abc1234"}`, string(body))
}

// TestProxyReusesUpstreamConnections: with Go's default transport the proxy
// kept 2 idle connections per upstream, so under concurrency it closed
// almost every connection after its response and dialled a new one for the
// next request; at about 770 requests/s the gateway ran out of local ports
// (connect: cannot assign requested address) and answered 502 (finding #96).
func TestProxyReusesUpstreamConnections(t *testing.T) {
	var dialled atomic.Int64
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond) // keeps the clients overlapping
		_, _ = w.Write([]byte(`{}`))
	}))
	upstream.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialled.Add(1)
		}
	}
	upstream.Start()
	t.Cleanup(upstream.Close)
	gw, _ := newGatewayUnderTest(t, upstream.URL)

	const clients, perClient = 50, 20
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: clients}}
	var wg sync.WaitGroup
	var failed atomic.Int64
	for range clients {
		wg.Go(func() {
			for range perClient {
				resp, err := client.Get(gw.URL + "/api/v1/leaderboard")
				if err != nil || resp.StatusCode != http.StatusOK {
					failed.Add(1)
				}
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}
		})
	}
	wg.Wait()

	require.Zero(t, failed.Load())
	t.Logf("%d requests from %d clients: %d upstream connections", clients*perClient, clients, dialled.Load())
	assert.LessOrEqual(t, dialled.Load(), int64(2*clients),
		"%d requests from %d clients opened %d upstream connections", clients*perClient, clients, dialled.Load())
}

func TestUpstreamDownReturns502(t *testing.T) {
	// Point every route at a port nothing listens on.
	gw, _ := newGatewayUnderTest(t, "http://127.0.0.1:1")

	resp, err := http.Get(gw.URL + "/api/v1/leaderboard")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "upstream unavailable")
}
