package matchmaking

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

func newHandlerRouter(t *testing.T) (*gin.Engine, *Service) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := NewService(
		NewQueue(rdb),
		NewRoomStore(rdb, time.Hour),
		&capturingPublisher{},
		metrics.New("mm-handler-test"),
		zap.NewNop(),
		Config{RankWindow: 100, BatchSize: 1000, MaxQueueAge: 5 * time.Minute},
	)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(svc).RegisterRoutes(r)
	return r, svc
}

func doReq(r *gin.Engine, method, path, body, userID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set(middleware.HeaderUserID, userID)
	}
	r.ServeHTTP(w, req)
	return w
}

func TestQueueJoinStatusLeave(t *testing.T) {
	r, _ := newHandlerRouter(t)

	w := doReq(r, http.MethodPost, "/queue", `{"rank":1000}`, "alice")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"queued"`)

	w = doReq(r, http.MethodGet, "/queue/status", "", "alice")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"queued":true`)
	assert.Contains(t, w.Body.String(), `"queue_size":1`)

	w = doReq(r, http.MethodDelete, "/queue", "", "alice")
	require.Equal(t, http.StatusOK, w.Code)

	// Leaving twice → 404.
	w = doReq(r, http.MethodDelete, "/queue", "", "alice")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestQueueRequiresIdentityAndValidRank(t *testing.T) {
	r, _ := newHandlerRouter(t)

	assert.Equal(t, http.StatusUnauthorized, doReq(r, http.MethodPost, "/queue", `{"rank":1000}`, "").Code)
	assert.Equal(t, http.StatusBadRequest, doReq(r, http.MethodPost, "/queue", `{"rank":-5}`, "alice").Code)
	assert.Equal(t, http.StatusBadRequest, doReq(r, http.MethodPost, "/queue", `not json`, "alice").Code)
}

func TestGetRoomEndpoint(t *testing.T) {
	r, svc := newHandlerRouter(t)
	ctx := context.Background()

	// Produce a real room through the matcher.
	require.NoError(t, svc.JoinQueue(ctx, "alice", 1000))
	require.NoError(t, svc.JoinQueue(ctx, "bob", 1010))
	_, err := svc.MatchOnce(ctx)
	require.NoError(t, err)

	pub := svc.pub.(*capturingPublisher)
	require.NotEmpty(t, pub.events)

	w := doReq(r, http.MethodGet, "/rooms/missing", "", "alice")
	assert.Equal(t, http.StatusNotFound, w.Code)
}
