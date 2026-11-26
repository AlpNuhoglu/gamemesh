package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

// countingUpstream answers 200 and counts the requests that reached it, by
// path, so a test can tell "refused at the gateway" from "refused upstream".
type countingUpstream struct {
	*httptest.Server
	hits atomic.Int64
	last atomic.Value // the path and identity of the last request
}

type upstreamRequest struct{ method, path, userID string }

func newCountingUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	u := &countingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.last.Store(upstreamRequest{r.Method, r.URL.Path, r.Header.Get(middleware.HeaderUserID)})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(u.Close)
	return u
}

// newGameClientGateway is the gateway with the release flag as given and a
// guest allowance of three an hour. It trusts the loopback proxy so a test
// can pose as different client addresses.
func newGameClientGateway(t *testing.T, upstreamURL string, enabled bool) (*httptest.Server, *auth.TokenManager) {
	t.Helper()
	cfg := config.Load("gateway")
	cfg.PlayerServiceURL = upstreamURL
	cfg.MatchmakingServiceURL = upstreamURL
	cfg.LeaderboardServiceURL = upstreamURL
	cfg.WebsocketServiceURL = upstreamURL
	cfg.GameClientEnabled = enabled
	cfg.GuestCreatePerHour = 3

	tokens := auth.NewTokenManager("gw-test-secret", time.Hour, "gamemesh")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	require.NoError(t, r.SetTrustedProxies([]string{"127.0.0.1"}))
	r.Use(middleware.RequestID())
	RegisterRoutes(r, cfg, tokens, nil, nil, zap.NewNop())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, tokens
}

// call sends one request, optionally with a bearer token and as a given
// client address, and returns the status.
func call(t *testing.T, method, url, token, clientIP string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// With the release flag off the new routes do not exist: nothing reaches the
// player service, whoever asks.
func TestGameClientRoutesAreAbsentWithoutTheFlag(t *testing.T) {
	upstream := newCountingUpstream(t)
	gw, tokens := newGameClientGateway(t, upstream.URL, false)
	token, _, err := tokens.Generate(uuid.New(), "alice")
	require.NoError(t, err)

	assert.Equal(t, http.StatusNotFound, call(t, http.MethodPost, gw.URL+"/api/v1/auth/guest", "", ""))
	assert.Equal(t, http.StatusNotFound, call(t, http.MethodDelete, gw.URL+"/api/v1/players/me", token, ""))
	assert.Zero(t, upstream.hits.Load(), "a request for a route behind the flag reached the upstream")
}

func TestGuestCreationIsLimitedPerIPByTheHour(t *testing.T) {
	upstream := newCountingUpstream(t)
	gw, _ := newGameClientGateway(t, upstream.URL, true)
	const here, there = "203.0.113.7", "203.0.113.8"
	guest := gw.URL + "/api/v1/auth/guest"

	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusOK, call(t, http.MethodPost, guest, "", here), "creation %d of the hour", i+1)
	}
	assert.Equal(t, upstreamRequest{http.MethodPost, "/auth/guest", ""}, upstream.last.Load())

	assert.Equal(t, http.StatusTooManyRequests, call(t, http.MethodPost, guest, "", here), "the fourth of the hour")
	assert.Equal(t, int64(3), upstream.hits.Load(), "a refused creation reached the player service")

	// The allowance is per address, and it is for creation only: the same
	// address can still log in (which is also how a guest renews).
	assert.Equal(t, http.StatusOK, call(t, http.MethodPost, guest, "", there))
	assert.Equal(t, http.StatusOK, call(t, http.MethodPost, gw.URL+"/api/v1/auth/login", "", here))
}

func TestGuestsAreKeptOffMemberRoutes(t *testing.T) {
	upstream := newCountingUpstream(t)
	gw, tokens := newGameClientGateway(t, upstream.URL, true)
	id := uuid.New()
	guest, err := tokens.GenerateGuest(id, "SwiftFox4821", time.Hour)
	require.NoError(t, err)
	member, _, err := tokens.Generate(uuid.New(), "alice")
	require.NoError(t, err)

	closed := []struct{ method, path string }{
		{http.MethodPut, "/api/v1/players/" + id.String()},
		{http.MethodPost, "/api/v1/score"},
	}
	for _, route := range closed {
		assert.Equal(t, http.StatusForbidden, call(t, route.method, gw.URL+route.path, guest.Token, ""), "guest: %s %s", route.method, route.path)
	}
	assert.Zero(t, upstream.hits.Load(), "a guest's request to a member route reached the upstream")
	for _, route := range closed {
		assert.Equal(t, http.StatusOK, call(t, route.method, gw.URL+route.path, member, ""), "member: %s %s", route.method, route.path)
	}

	// What a guest is for stays open to it.
	open := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/queue"},
		{http.MethodGet, "/api/v1/players/me"},
		{http.MethodPost, "/api/v1/auth/logout"},
	}
	for _, route := range open {
		assert.Equal(t, http.StatusOK, call(t, route.method, gw.URL+route.path, guest.Token, ""), "guest: %s %s", route.method, route.path)
	}
}

func TestDeleteAccountIsProxiedWithTheCallersIdentity(t *testing.T) {
	upstream := newCountingUpstream(t)
	gw, tokens := newGameClientGateway(t, upstream.URL, true)
	id := uuid.New()
	guest, err := tokens.GenerateGuest(id, "SwiftFox4821", time.Hour)
	require.NoError(t, err)

	assert.Equal(t, http.StatusUnauthorized, call(t, http.MethodDelete, gw.URL+"/api/v1/players/me", "", ""))
	assert.Zero(t, upstream.hits.Load())

	assert.Equal(t, http.StatusOK, call(t, http.MethodDelete, gw.URL+"/api/v1/players/me", guest.Token, ""))
	assert.Equal(t, upstreamRequest{http.MethodDelete, "/players/me", id.String()}, upstream.last.Load())
}
