package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
}

// newGatewayUnderTest runs the gateway engine in a real HTTP server:
// httputil.ReverseProxy needs a live ResponseWriter (httptest.ResponseRecorder
// does not implement CloseNotifier through gin).
func newGatewayUnderTest(t *testing.T, upstreamURL string) (*httptest.Server, *auth.TokenManager) {
	t.Helper()

	cfg := config.Load("gateway")
	cfg.PlayerServiceURL = upstreamURL
	cfg.MatchmakingServiceURL = upstreamURL
	cfg.LeaderboardServiceURL = upstreamURL
	cfg.WebsocketServiceURL = upstreamURL

	tokens := auth.NewTokenManager("gw-test-secret", time.Hour, "gamemesh")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	RegisterRoutes(r, cfg, tokens, zap.NewNop())

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
