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

	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
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
