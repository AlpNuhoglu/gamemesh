//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/player"
	"github.com/alpnuhoglu/gamemesh/pkg/auth"
)

// TestPlayerAPIEndToEnd exercises the full HTTP stack of the player service
// against real PostgreSQL and Redis: register → login → fetch profile.
func TestPlayerAPIEndToEnd(t *testing.T) {
	db := startPostgres(t)
	rdb := startRedis(t)

	tokens := auth.NewTokenManager("integration-secret", time.Hour, "gamemesh")
	svc := player.NewService(
		newRepo(db),
		player.NewSessionStore(rdb),
		tokens,
		zap.NewNop(),
	)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	player.NewHandler(svc).RegisterRoutes(engine)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	// Register.
	resp := postJSON(t, srv.URL+"/auth/register", map[string]any{
		"username": "alice",
		"email":    "alice@example.com",
		"password": "password123",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created struct {
		ID string `json:"id"`
	}
	decode(t, resp, &created)
	require.NotEmpty(t, created.ID)

	// Duplicate registration is rejected.
	resp = postJSON(t, srv.URL+"/auth/register", map[string]any{
		"username": "alice",
		"email":    "alice@example.com",
		"password": "password123",
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()

	// Login.
	resp = postJSON(t, srv.URL+"/auth/login", map[string]any{
		"identifier": "alice",
		"password":   "password123",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var login struct {
		Token string `json:"token"`
	}
	decode(t, resp, &login)
	require.NotEmpty(t, login.Token)

	// The issued token is valid and carries the player ID.
	claims, err := tokens.Validate(login.Token)
	require.NoError(t, err)
	assert.Equal(t, created.ID, claims.PlayerID)

	// The login cached a session in Redis.
	exists, err := player.NewSessionStore(rdb).Exists(context.Background(), claims.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	// Fetch profile (gateway would set X-User-ID after JWT validation).
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/players/"+created.ID, nil)
	req.Header.Set("X-User-ID", created.ID)
	getResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	var profile struct {
		Username string `json:"username"`
		Stats    *struct {
			Rank int `json:"rank"`
		} `json:"stats"`
	}
	decode(t, getResp, &profile)
	assert.Equal(t, "alice", profile.Username)
	require.NotNil(t, profile.Stats)
	assert.Equal(t, 1000, profile.Stats.Rank)

	// Wrong password is a 401.
	resp = postJSON(t, srv.URL+"/auth/login", map[string]any{
		"identifier": "alice",
		"password":   "wrong",
	})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	require.NoError(t, err, fmt.Sprintf("POST %s", url))
	return resp
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(into))
}
