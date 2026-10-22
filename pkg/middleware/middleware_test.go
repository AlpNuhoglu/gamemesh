package middleware

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
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

// TestMetricsLabelsStayBounded: the method and path come from the client, so
// made-up methods and random paths must land in a fixed set of series instead
// of opening one per value (finding #90: Traefik forwards any method token).
func TestMetricsLabelsStayBounded(t *testing.T) {
	m := metrics.New("test-middleware-bounded")
	r := newEngine(Metrics(m))

	serve := func(method, path string) {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	}
	serve(http.MethodGet, "/test")
	for i := range 50 {
		serve(fmt.Sprintf("ZZCHECK%d", i), "/test")
		serve(http.MethodGet, fmt.Sprintf("/random/%d", i))
		serve(fmt.Sprintf("zz%d", i), fmt.Sprintf("/random/%d", i))
	}

	// GET /test 200, GET unmatched 404, other unmatched 404.
	assert.Equal(t, 3, countSeries(m.RequestsTotal))
	assert.Equal(t, 2, countSeries(m.ErrorsTotal))
	assert.Equal(t, 3, countSeries(m.RequestDuration))

	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, w.Body.String(),
		`gamemesh_http_requests_total{method="other",path="unmatched",service="test-middleware-bounded",status="404"} 100`)
}

// TestMetricsWebSocketSessions: a request whose connection is hijacked (the
// WebSocket handshake, in the websocket service and through the gateway's
// proxy) returns from the handler only when the session ends, so its "latency"
// is the session's lifetime (finding #92). It is counted as 101 and kept out of
// the latency histogram; Upgrade headers alone change nothing.
func TestMetricsWebSocketSessions(t *testing.T) {
	m := metrics.New("test-middleware-ws")
	r := newEngine(Metrics(m))
	r.GET("/ws", func(c *gin.Context) {
		conn, rw, err := c.Writer.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		time.Sleep(50 * time.Millisecond) // the session
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, path := range []string{"/ws", "/test"} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		require.NoError(t, err)
		_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", path)
		require.NoError(t, err)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		require.NoError(t, err)
		_ = resp.Body.Close()
		_ = conn.Close()
	}

	// The /ws handler records after the session ends.
	require.Eventually(t, func() bool { return countSeries(m.RequestsTotal) == 2 }, time.Second, 10*time.Millisecond)
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	assert.Contains(t, body, `gamemesh_http_requests_total{method="GET",path="/ws",service="test-middleware-ws",status="101"} 1`)
	assert.Contains(t, body, `gamemesh_http_requests_total{method="GET",path="/test",service="test-middleware-ws",status="200"} 1`)
	assert.Equal(t, 1, countSeries(m.RequestDuration))
	assert.NotContains(t, body, `gamemesh_http_request_duration_seconds_count{method="GET",path="/ws"`)
}

// countSeries returns how many label combinations c currently exports.
func countSeries(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 1024)
	c.Collect(ch)
	close(ch)
	return len(ch)
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
	r.Use(Auth(tm, nil)) // nil sessions → JWT-only (revocation check disabled)
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

// stubSessions is a fixed-verdict SessionChecker for the revocation tests.
type stubSessions struct {
	active bool
	err    error
}

func (s stubSessions) Exists(context.Context, string) (bool, error) { return s.active, s.err }

func TestAuthMiddlewareRevocation(t *testing.T) {
	tm := auth.NewTokenManager("test-secret", time.Hour, "gamemesh")
	token, _, err := tm.Generate(uuid.New(), "alice")
	require.NoError(t, err)

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/protected", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return req
	}
	run := func(sessions SessionChecker) int {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(Auth(tm, sessions))
		r.GET("/protected", func(c *gin.Context) { c.JSON(200, gin.H{}) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, newReq())
		return w.Code
	}

	// Active session → allowed.
	assert.Equal(t, http.StatusOK, run(stubSessions{active: true}))
	// Revoked (logged-out) session → 401, even though the JWT is cryptographically valid.
	assert.Equal(t, http.StatusUnauthorized, run(stubSessions{active: false}))
	// Store unavailable → fail closed (503), never let a possibly-revoked token through.
	assert.Equal(t, http.StatusServiceUnavailable, run(stubSessions{err: errors.New("redis down")}))
}

// probeEngine serves the probe paths plus /test, with /readyz failing.
func probeEngine(mw ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(mw...)
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	for _, p := range []string{"/livez", "/healthz", "/metrics", "/health", "/test"} {
		r.GET(p, ok)
	}
	r.GET("/readyz", func(c *gin.Context) { c.Status(http.StatusServiceUnavailable) })
	return r
}

var probeAndWorkPaths = []string{"/livez", "/healthz", "/metrics", "/health", "/readyz", "/test"}

// TestLoggerSkipsSuccessfulProbes: successful probes stay out of the access
// log; a failing one is still logged, since its body names the broken
// dependency and appears nowhere else.
func TestLoggerSkipsSuccessfulProbes(t *testing.T) {
	obs, logs := observer.New(zapcore.DebugLevel)
	r := probeEngine(RequestID(), Logger(zap.New(obs), time.Second))
	for _, p := range probeAndWorkPaths {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}

	var logged []string
	for _, e := range logs.All() {
		logged = append(logged, e.ContextMap()["path"].(string))
	}
	assert.Equal(t, []string{"/readyz", "/test"}, logged)
}

func TestMetricsSkipsProbes(t *testing.T) {
	m := metrics.New("test-middleware-probes")
	r := probeEngine(Metrics(m))
	for _, p := range probeAndWorkPaths {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}

	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	assert.Contains(t, body, `path="/test"`)
	for _, p := range probeAndWorkPaths[:5] {
		assert.NotContains(t, body, `path="`+p+`"`, p)
	}
}

// TestRateLimitExemptsInternalProbes: the exemption used to hold only because
// the gateway registered /healthz and /metrics before installing the limiter
// (gin fixes a route's middleware chain when the route is registered). Here
// the probes are registered after RateLimit and must still never be
// throttled, while the public /health is limited like any other route.
func TestRateLimitExemptsInternalProbes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RateLimit(1, 1))
	paths := []string{"/livez", "/readyz", "/healthz", "/metrics", "/health"}
	for _, p := range paths {
		r.GET(p, func(c *gin.Context) { c.Status(http.StatusOK) })
	}

	got := map[string][]int{}
	for _, p := range paths {
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.RemoteAddr = "10.0.0.9:1234"
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			got[p] = append(got[p], w.Code)
		}
	}
	for _, p := range paths[:4] {
		assert.Equal(t, []int{200, 200, 200}, got[p], p)
	}
	assert.Equal(t, []int{200, 429, 429}, got["/health"])
}
