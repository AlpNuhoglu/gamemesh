package leaderboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

type nopPublisher struct {
	mu    sync.Mutex
	count int
}

func (p *nopPublisher) Publish(context.Context, string, events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	return nil
}
func (p *nopPublisher) Close() error { return nil }

func newTestRouter(t *testing.T) (*gin.Engine, *nopPublisher) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pub := &nopPublisher{}
	svc := NewService(NewStore(rdb), pub, metrics.New("lb-handler-test"), zap.NewNop())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(svc).RegisterRoutes(r)
	return r, pub
}

func do(r *gin.Engine, method, path, body, userID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set(middleware.HeaderUserID, userID)
	}
	r.ServeHTTP(w, req)
	return w
}

func TestSubmitScoreEndpoint(t *testing.T) {
	r, pub := newTestRouter(t)

	w := do(r, http.MethodPost, "/score", `{"score":100}`, "player-1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"rank":1`)
	assert.Equal(t, 1, pub.count, "score update publishes an event")

	// Increment mode.
	w = do(r, http.MethodPost, "/score", `{"score":50,"increment":true}`, "player-1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"score":150`)
}

func TestSubmitScoreRequiresIdentity(t *testing.T) {
	r, _ := newTestRouter(t)
	w := do(r, http.MethodPost, "/score", `{"score":100}`, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSubmitScoreValidation(t *testing.T) {
	r, _ := newTestRouter(t)
	w := do(r, http.MethodPost, "/score", `{"not":"valid"}`, "player-1")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLeaderboardEndpoints(t *testing.T) {
	r, _ := newTestRouter(t)
	for i, p := range []string{"p1", "p2", "p3"} {
		w := do(r, http.MethodPost, "/score", `{"score":`+string(rune('1'+i))+`00}`, p)
		require.Equal(t, http.StatusOK, w.Code)
	}

	w := do(r, http.MethodGet, "/leaderboard/top/2", "", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "p3")

	w = do(r, http.MethodGet, "/leaderboard?offset=0&limit=10", "", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"total":3`)

	w = do(r, http.MethodGet, "/leaderboard/rank/p1", "", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"rank":3`)

	w = do(r, http.MethodGet, "/leaderboard/rank/ghost", "", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestLeaderboardBoundsValidation(t *testing.T) {
	r, _ := newTestRouter(t)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/leaderboard/top/0", "", "").Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/leaderboard/top/9999", "", "").Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/leaderboard?limit=-1", "", "").Code)
}
