package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

func TestNewEngineHealthAndMetrics(t *testing.T) {
	cfg := config.Load("testsvc")
	engine := NewEngine(cfg, zap.NewNop(), metrics.New("testsvc"))

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"testsvc"`)

	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "go_goroutines")
}

func TestHealthEndpointsWithoutChecks(t *testing.T) {
	engine := NewEngine(config.Load("testsvc"), zap.NewNop(), metrics.New("testsvc-health"))
	for _, path := range []string{"/livez", "/readyz", "/healthz"} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, w.Code, path)
		assert.JSONEq(t, `{"status":"ok","service":"testsvc"}`, w.Body.String(), path)
	}
}

// probe returns the status code of each path and the body of the last one.
func probe(engine *gin.Engine, paths ...string) (map[string]int, string) {
	codes := make(map[string]int, len(paths))
	var body string
	for _, path := range paths {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		codes[path], body = w.Code, w.Body.String()
	}
	return codes, body
}

// A failing dependency takes the pod out of rotation (/readyz 503) but must
// not get it restarted (/livez and /healthz stay 200).
func TestFailingReadinessKeepsLiveness(t *testing.T) {
	engine := NewEngine(config.Load("testsvc"), zap.NewNop(), metrics.New("testsvc-ready"),
		WithReadiness("redis", func(context.Context) error { return errors.New("dial tcp: connection refused") }),
		WithReadiness("postgres", func(context.Context) error { return nil }))

	codes, body := probe(engine, "/livez", "/healthz", "/readyz")
	assert.Equal(t, map[string]int{"/livez": 200, "/healthz": 200, "/readyz": 503}, codes)
	assert.JSONEq(t, `{"status":"unavailable","service":"testsvc",
		"checks":{"redis":"dial tcp: connection refused","postgres":"ok"}}`, body)
}

func TestFailingLivenessFailsLivez(t *testing.T) {
	engine := NewEngine(config.Load("testsvc"), zap.NewNop(), metrics.New("testsvc-live"),
		WithLiveness("match-loop", func(context.Context) error { return errors.New("no heartbeat for 45s") }))

	codes, _ := probe(engine, "/readyz", "/healthz", "/livez")
	assert.Equal(t, map[string]int{"/readyz": 200, "/healthz": 503, "/livez": 503}, codes)
}

// A check that ignores its context must not hang the probe.
func TestHealthCheckTimesOut(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	engine := NewEngine(config.Load("testsvc"), zap.NewNop(), metrics.New("testsvc-stuck"),
		WithReadiness("stuck", func(context.Context) error { <-block; return nil }))

	start := time.Now()
	codes, body := probe(engine, "/readyz")
	assert.Less(t, time.Since(start), 2*checkTimeout)
	assert.Equal(t, http.StatusServiceUnavailable, codes["/readyz"])
	assert.Contains(t, body, `"stuck":"timed out after 1s"`)
}

// Checks run concurrently: two that each take most of the shared budget
// still both pass.
func TestHealthChecksRunConcurrently(t *testing.T) {
	slow := func(ctx context.Context) error {
		select {
		case <-time.After(600 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	engine := NewEngine(config.Load("testsvc"), zap.NewNop(), metrics.New("testsvc-parallel"),
		WithReadiness("a", slow), WithReadiness("b", slow))

	codes, _ := probe(engine, "/readyz")
	assert.Equal(t, http.StatusOK, codes["/readyz"])
}

// TestHTTPTracePropagation asserts that trace context survives the
// gateway->service HTTP boundary: a client span made with the otelhttp
// transport (as the gateway proxy uses) is continued by the otelgin server
// middleware in NewEngine, so both sides share one trace id.
func TestHTTPTracePropagation(t *testing.T) {
	// A recording provider is required so client spans are sampled and have a
	// valid (propagatable) SpanContext.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	cfg := config.Load("testsvc")
	engine := NewEngine(cfg, zap.NewNop(), metrics.New("testsvc-trace"))

	var serverTrace trace.TraceID
	engine.GET("/echo", func(c *gin.Context) {
		serverTrace = trace.SpanContextFromContext(c.Request.Context()).TraceID()
		c.Status(http.StatusOK)
	})

	srv := httptest.NewServer(engine)
	defer srv.Close()

	// Client side: start a parent span, then call through the otelhttp transport
	// (mirrors internal/gateway/proxy.go's proxy.Transport).
	ctx, parent := otel.Tracer("test").Start(context.Background(), "client")
	clientTrace := parent.SpanContext().TraceID()

	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/echo", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	parent.End()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, serverTrace.IsValid(), "server must see a valid trace context")
	assert.Equal(t, clientTrace, serverTrace, "trace id must survive the HTTP boundary")
}

// TestRateLimitIgnoresSpoofedForwardingHeaders: with gin's default of trusting
// every proxy, a client could send a new X-Forwarded-For (or X-Real-IP) value
// per request and get a fresh rate-limit bucket each time. With no trusted
// proxies configured, the limiter must key on the peer address only.
func TestRateLimitIgnoresSpoofedForwardingHeaders(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP"} {
		t.Run(header, func(t *testing.T) {
			cfg := config.Load("testsvc")
			engine := NewEngine(cfg, zap.NewNop(), metrics.New("testsvc-ratelimit"))
			engine.Use(middleware.RateLimit(1, 1)) // burst 1: the second request is over the limit
			engine.GET("/limited", func(c *gin.Context) { c.Status(http.StatusOK) })

			codes := make([]int, 0, 2)
			for _, spoofed := range []string{"6.6.6.1", "6.6.6.2"} {
				req := httptest.NewRequest(http.MethodGet, "/limited", nil)
				req.RemoteAddr = "203.0.113.9:4000"
				req.Header.Set(header, spoofed)
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, req)
				codes = append(codes, w.Code)
			}
			assert.Equal(t, []int{http.StatusOK, http.StatusTooManyRequests}, codes,
				"a new %s value must not buy a new rate-limit bucket", header)
		})
	}
}

// TestNewEngineClientIP pins how the client IP is resolved for each
// TRUSTED_PROXIES setting.
func TestNewEngineClientIP(t *testing.T) {
	cases := []struct {
		name    string
		trusted []string
		peer    string
		xff     string
		want    string
	}{
		{"no trusted proxy: header ignored", nil, "203.0.113.9:4000", "6.6.6.6", "203.0.113.9"},
		{"trusted proxy: nearest untrusted hop wins", []string{"10.42.0.0/16"}, "10.42.0.7:4000", "6.6.6.6, 203.0.113.9", "203.0.113.9"},
		{"untrusted peer: header ignored", []string{"10.42.0.0/16"}, "198.51.100.5:4000", "6.6.6.6", "198.51.100.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Load("testsvc")
			cfg.TrustedProxies = tc.trusted
			engine := NewEngine(cfg, zap.NewNop(), metrics.New("testsvc-clientip"))
			var got string
			engine.GET("/ip", func(c *gin.Context) { got = c.ClientIP() })

			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = tc.peer
			req.Header.Set("X-Forwarded-For", tc.xff)
			engine.ServeHTTP(httptest.NewRecorder(), req)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNewEngineFailsFastOnInvalidTrustedProxies(t *testing.T) {
	cfg := config.Load("testsvc")
	cfg.TrustedProxies = []string{"not-an-ip"}
	// Fatal would exit the test binary; make it panic instead.
	log := zap.NewNop().WithOptions(zap.WithFatalHook(zapcore.WriteThenPanic))
	assert.Panics(t, func() { NewEngine(cfg, log, metrics.New("testsvc-badproxy")) })
}

// startRun runs Run on a free port and returns once it accepts connections,
// so a signal sent afterwards is caught by Run instead of killing the test.
func startRun(t *testing.T, engine *gin.Engine, hooks ...func(context.Context)) (addr string, done <-chan error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	ch := make(chan error, 1)
	go func() { ch <- Run(engine, strconv.Itoa(port), zap.NewNop(), hooks...) }()
	addr = "127.0.0.1:" + strconv.Itoa(port)
	require.Eventually(t, func() bool {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	return addr, ch
}

func TestRunRunsShutdownHooks(t *testing.T) {
	engine := NewEngine(config.Load("testsvc"), zap.NewNop(), metrics.New("testsvc-hooks"))
	hookRan := make(chan bool, 1)
	_, done := startRun(t, engine, func(ctx context.Context) {
		_, hasDeadline := ctx.Deadline()
		hookRan <- hasDeadline
	})

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	require.NoError(t, <-done)
	assert.True(t, <-hookRan, "the hook gets the shutdown budget as its deadline")
}

// TestRunReportsShutdownTimeout: when in-flight work outlives the budget, Run
// returns an error instead of exiting, so main's deferred cleanup still runs.
func TestRunReportsShutdownTimeout(t *testing.T) {
	prev := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = prev })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	engine := NewEngine(config.Load("testsvc"), zap.NewNop(), metrics.New("testsvc-timeout"))
	engine.GET("/slow", func(*gin.Context) { <-release })
	addr, done := startRun(t, engine)
	go func() { _, _ = http.Get("http://" + addr + "/slow") }()
	time.Sleep(100 * time.Millisecond) // let the request reach the handler

	start := time.Now()
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	err := <-done
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
}

func TestRunFailsFastOnBadPort(t *testing.T) {
	cfg := config.Load("testsvc")
	engine := NewEngine(cfg, zap.NewNop(), metrics.New("testsvc-run"))
	err := Run(engine, "999999", zap.NewNop())
	assert.Error(t, err)
}

func TestShutdownContext(t *testing.T) {
	ctx, cancel := ShutdownContext()
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("context must not be cancelled before a signal")
	default:
	}
}

// TestProbesAreNotTraced: probes and /metrics scrapes produced a trace per
// request (hundreds every 10 minutes for one service); only real work should.
func TestProbesAreNotTraced(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	engine := NewEngine(config.Load("testsvc"), zap.NewNop(), metrics.New("testsvc-traces"))
	engine.GET("/work", func(c *gin.Context) { c.Status(http.StatusOK) })
	for _, p := range []string{"/livez", "/readyz", "/healthz", "/metrics", "/work"} {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}

	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Contains(t, spans[0].Name(), "/work")
}

func TestWorkersWait(t *testing.T) {
	var w Workers
	w.Go(func() {})
	w.Go(func() { time.Sleep(20 * time.Millisecond) })
	assert.True(t, w.Wait(time.Second))

	var stuck Workers
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stuck.Go(func() { <-release })
	start := time.Now()
	assert.False(t, stuck.Wait(50*time.Millisecond))
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}
