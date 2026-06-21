package presence

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newHandlerRouter(t *testing.T) *gin.Engine {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc := NewService(NewRepository(rdb, testTTL), &capturingPublisher{}, nil, zap.NewNop())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(svc).RegisterRoutes(r)
	return r
}

func do(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestHandlerConnectThenGet(t *testing.T) {
	r := newHandlerRouter(t)

	w := do(r, http.MethodPost, "/presence/connect", `{"player_id":"alice"}`)
	require.Equal(t, http.StatusOK, w.Code)

	w = do(r, http.MethodGet, "/presence/alice", "")
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ONLINE", body["state"])
}

func TestHandlerGetUnknownIsOffline(t *testing.T) {
	r := newHandlerRouter(t)
	w := do(r, http.MethodGet, "/presence/nobody", "")
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "OFFLINE", body["state"])
}

func TestHandlerSetStateInvalidTransitionIs400(t *testing.T) {
	r := newHandlerRouter(t)
	w := do(r, http.MethodPut, "/presence/state", `{"player_id":"ghost","state":"IN_MATCH"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandlerFriendsBulk(t *testing.T) {
	r := newHandlerRouter(t)
	require.Equal(t, http.StatusOK, do(r, http.MethodPost, "/presence/connect", `{"player_id":"a"}`).Code)

	w := do(r, http.MethodPost, "/presence/friends", `{"ids":["a","b"]}`)
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Friends []Friend `json:"friends"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Friends, 2)
	assert.Equal(t, StateOnline, body.Friends[0].State)
	assert.Equal(t, StateOffline, body.Friends[1].State)
}

func TestHandlerFriendsRejectsMissingIDs(t *testing.T) {
	r := newHandlerRouter(t)
	w := do(r, http.MethodPost, "/presence/friends", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
