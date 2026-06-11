package middleware

import (
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
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

func newEngine(mw ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(mw...)
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	return r
}

func TestRequestIDGeneratedAndPropagated(t *testing.T) {
	r := newEngine(RequestID())

	// Generated when absent.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	generated := w.Header().Get(HeaderRequestID)
	assert.NotEmpty(t, generated)

	// Preserved when present.
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(HeaderRequestID, "fixed-id")
	r.ServeHTTP(w, req)
	assert.Equal(t, "fixed-id", w.Header().Get(HeaderRequestID))
}

func TestLoggerDoesNotInterfere(t *testing.T) {
	r := newEngine(RequestID(), Logger(zap.NewNop()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMetricsCountsRequests(t *testing.T) {
	m := metrics.New("test-middleware")
	r := newEngine(Metrics(m))

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	}
	// 404s count as errors too.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Scrape our own registry and assert the counters appear.
	w = httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	assert.Contains(t, body, `gamemesh_http_requests_total`)
	assert.Contains(t, body, `gamemesh_http_errors_total`)
}

func TestCORSAllowAll(t *testing.T) {
	r := newEngine(CORS([]string{"*"}))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://game.example.com")
	r.ServeHTTP(w, req)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSAllowList(t *testing.T) {
	r := newEngine(CORS([]string{"https://allowed.example.com"}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://allowed.example.com")
	r.ServeHTTP(w, req)
	assert.Equal(t, "https://allowed.example.com", w.Header().Get("Access-Control-Allow-Origin"))

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	r.ServeHTTP(w, req)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSPreflight(t *testing.T) {
	r := newEngine(CORS([]string{"*"}))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/test", nil)
	req.Header.Set("Origin", "https://game.example.com")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestRateLimitRejectsBurstOverflow(t *testing.T) {
	// 1 rps, burst 2 → third immediate request must be rejected.
	r := newEngine(RateLimit(1, 2))

	statuses := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		r.ServeHTTP(w, req)
		statuses = append(statuses, w.Code)
	}
	assert.Equal(t, []int{200, 200, 429}, statuses)

	// A different IP has its own bucket.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuthMiddleware(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", time.Hour, "gamemesh")
	playerID := uuid.New()
	token, jti, err := tm.Generate(playerID, "alice")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Auth(tm))
	r.GET("/protected", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"user_id": c.GetString(CtxUserID),
			"jti":     c.GetString(CtxTokenJTI),
		})
	})

	// No token.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/protected", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Malformed header.
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Token abc")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Invalid token.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Valid token populates identity.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), playerID.String())
	assert.Contains(t, w.Body.String(), jti)
}
