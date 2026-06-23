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
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

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
	r := newEngine(RequestID(), Logger(zap.NewNop(), time.Second))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestLoggerLevelRouting verifies the sampler-bypass contract at the middleware
// layer: a 2xx logs at Info (sampler-eligible), a 5xx at Error and a slow 2xx at
// Warn (both sampler-bypassed).
func TestLoggerLevelRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name      string
		path      string
		slowAfter time.Duration
		handler   gin.HandlerFunc
		wantLevel zapcore.Level
	}{
		{"ok 2xx", "/ok", time.Second, func(c *gin.Context) { c.JSON(200, gin.H{}) }, zapcore.InfoLevel},
		{"server 5xx", "/err", time.Second, func(c *gin.Context) { c.JSON(500, gin.H{}) }, zapcore.ErrorLevel},
		{"slow 2xx", "/slow", time.Nanosecond, func(c *gin.Context) { time.Sleep(time.Millisecond); c.JSON(200, gin.H{}) }, zapcore.WarnLevel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			log := zap.New(obs)
			r := gin.New()
			r.Use(RequestID(), Logger(log, tc.slowAfter))
			r.GET(tc.path, tc.handler)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))

			require.Equal(t, 1, logs.Len())
			assert.Equal(t, tc.wantLevel, logs.All()[0].Level)
		})
	}
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
