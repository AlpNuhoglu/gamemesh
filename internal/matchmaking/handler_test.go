package matchmaking

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestGetRoomEndpoint: a room is returned to its players only. A caller who
// is not one of them gets the same 404 as for a room that does not exist,
// and the refusal is counted; a caller without an identity gets 401.
func TestGetRoomEndpoint(t *testing.T) {
	r, svc := newHandlerRouter(t)
	ctx := context.Background()

	// Produce a real room through the matcher.
	require.NoError(t, svc.JoinQueue(ctx, "alice", 1000))
	require.NoError(t, svc.JoinQueue(ctx, "bob", 1010))
	_, err := svc.MatchOnce(ctx)
	require.NoError(t, err)

	pub := svc.pub.(*capturingPublisher)
	require.Len(t, pub.events, 1)
	var match events.MatchFoundPayload
	require.NoError(t, json.Unmarshal(pub.events[0].Payload, &match))
	path := "/rooms/" + match.RoomID

	for _, player := range []string{"alice", "bob"} {
		w := doReq(r, http.MethodGet, path, "", player)
		require.Equal(t, http.StatusOK, w.Code, player)
		var room Room
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &room), w.Body.String())
		assert.Equal(t, match.RoomID, room.ID)
		assert.ElementsMatch(t, []string{"alice", "bob"}, room.Players)
	}
	assert.Zero(t, roomDenied(t, svc))

	missing := doReq(r, http.MethodGet, "/rooms/missing", "", "alice")
	assert.Equal(t, http.StatusNotFound, missing.Code)
	assert.Zero(t, roomDenied(t, svc), "a missing room is not a denial")

	stranger := doReq(r, http.MethodGet, path, "", "mallory")
	assert.Equal(t, http.StatusNotFound, stranger.Code)
	assert.Equal(t, missing.Body.String(), stranger.Body.String(),
		"a stranger cannot tell a live room from a missing one")
	assert.NotContains(t, stranger.Body.String(), "alice")
	assert.Equal(t, float64(1), roomDenied(t, svc))

	assert.Equal(t, http.StatusUnauthorized, doReq(r, http.MethodGet, path, "", "").Code)
	assert.Equal(t, float64(1), roomDenied(t, svc))
}

// roomDenied scrapes gamemesh_matchmaking_room_denied_total.
func roomDenied(t *testing.T, svc *Service) float64 {
	t.Helper()
	w := httptest.NewRecorder()
	svc.m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	const series = `gamemesh_matchmaking_room_denied_total{service="mm-handler-test"} `
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series); ok {
			f, err := strconv.ParseFloat(v, 64)
			require.NoError(t, err)
			return f
		}
	}
	t.Fatalf("no series %q", series)
	return 0
}
