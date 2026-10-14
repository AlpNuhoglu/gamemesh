package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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
