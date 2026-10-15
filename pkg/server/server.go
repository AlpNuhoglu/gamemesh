// Package server provides shared HTTP bootstrap: the base middleware chain,
// health/metrics endpoints and graceful shutdown — identical across all five
// services, so it lives in one place.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
)

// Check reports whether one dependency or background worker is healthy. It
// must honour ctx: checks share a short deadline so that a slow dependency
// fails the probe instead of hanging it.
type Check func(ctx context.Context) error

// checkTimeout bounds one /livez or /readyz evaluation. It stays below the
// probe timeouts (readiness 2 s, liveness 3 s) so the kubelet always gets an
// answer.
const checkTimeout = time.Second

// Option configures NewEngine.
type Option func(*health)

type namedCheck struct {
	name  string
	check Check
}

type health struct {
	liveness  []namedCheck
	readiness []namedCheck
}

// WithReadiness adds a dependency the service needs to serve its requests.
// While it fails, /readyz returns 503 and Kubernetes stops routing to the pod
// without restarting it. Never pass another application service here: a
// readiness check on a downstream turns its outage into ours.
func WithReadiness(name string, c Check) Option {
	return func(h *health) { h.readiness = append(h.readiness, namedCheck{name, c}) }
}

// WithLiveness adds an in-process signal, such as a background loop's
// heartbeat. While it fails, /livez returns 503 and Kubernetes restarts the
// container, so it must never depend on anything outside the process:
// restarting does not fix a database outage, it multiplies it.
func WithLiveness(name string, c Check) Option {
	return func(h *health) { h.liveness = append(h.liveness, namedCheck{name, c}) }
}

// NewEngine builds a gin engine with the standard middleware chain plus the
// health endpoints and /metrics:
//   - /livez: the process can serve and its background loops are alive
//     (liveness checks only, no dependencies).
//   - /readyz: the readiness checks pass, i.e. the pod should get traffic.
//   - /healthz: alias of /livez, kept for existing callers.
func NewEngine(cfg *config.Config, log *zap.Logger, m *metrics.Metrics, opts ...Option) *gin.Engine {
	var h health
	for _, opt := range opts {
		opt(&h)
	}

	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	// gin trusts every proxy by default, so ClientIP() returns whatever the
	// client wrote into X-Forwarded-For / X-Real-IP — and the per-IP rate
	// limiter and access logs key on it. Trust only the configured proxies.
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		log.Fatal("invalid TRUSTED_PROXIES",
			zap.Strings("trusted_proxies", cfg.TrustedProxies), zap.Error(err))
	}
	r.Use(
		gin.Recovery(),
		middleware.RequestID(),
		// otelgin sits after RequestID (so request_id exists for logs) and
		// before Logger (so the span context is in c.Request.Context() when the
		// logger reads trace/span IDs). It extracts incoming W3C trace context,
		// continuing a trace started upstream, and records a server span per
		// request with http.method, http.route (low-cardinality template),
		// status and latency. Probe and /metrics requests are not traced.
		otelgin.Middleware(cfg.OTelServiceName, otelgin.WithFilter(func(r *http.Request) bool {
			return !middleware.IsTelemetryExcluded(r.URL.Path)
		})),
		middleware.Logger(log, cfg.LogSlowRequestThreshold),
		middleware.Metrics(m),
		middleware.CORS(cfg.AllowedOrigins),
	)
	livez := healthHandler(cfg.ServiceName, h.liveness)
	r.GET("/livez", livez)
	r.GET("/healthz", livez)
	r.GET("/readyz", healthHandler(cfg.ServiceName, h.readiness))
	r.GET("/metrics", gin.WrapH(m.Handler()))
	return r
}

// healthResponse is the body of /livez, /readyz and /healthz. Check results
// name the failing dependency and its error; these endpoints are internal
// and must never be routed from the internet.
type healthResponse struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Checks  map[string]string `json:"checks,omitempty"`
}

func healthHandler(service string, checks []namedCheck) gin.HandlerFunc {
	return func(c *gin.Context) {
		results, ok := runChecks(c.Request.Context(), checks)
		resp := healthResponse{Status: "ok", Service: service, Checks: results}
		code := http.StatusOK
		if !ok {
			resp.Status, code = "unavailable", http.StatusServiceUnavailable
		}
		c.JSON(code, resp)
	}
}

// runChecks runs every check concurrently under one checkTimeout deadline. A
// check that has not answered by then is reported as timed out, even if it
// ignores ctx, so a stuck dependency can never hang the probe.
func runChecks(ctx context.Context, checks []namedCheck) (map[string]string, bool) {
	if len(checks) == 0 {
		return nil, true
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	type result struct {
		name string
		err  error
	}
	done := make(chan result, len(checks)) // buffered: late checks never block
	for _, nc := range checks {
		go func() { done <- result{nc.name, nc.check(ctx)} }()
	}

	results := make(map[string]string, len(checks))
	for _, nc := range checks {
		results[nc.name] = "timed out after " + checkTimeout.String()
	}
	ok := true
	for range checks {
		select {
		case r := <-done:
			if r.err != nil {
				results[r.name], ok = r.err.Error(), false
			} else {
				results[r.name] = "ok"
			}
		case <-ctx.Done():
			return results, false
		}
	}
	return results, ok
}

// shutdownTimeout bounds the whole graceful shutdown of the HTTP server and
// the shutdown hooks. With a 5 s preStop and a few seconds for background
// workers it fits in Kubernetes' default 30 s termination grace period.
var shutdownTimeout = 15 * time.Second

// Run serves the engine until SIGINT/SIGTERM, then shuts down gracefully
// within shutdownTimeout: it stops accepting connections and waits for
// in-flight requests, and runs the hooks at the same time. Hooks are for what
// http.Server.Shutdown does not track, such as hijacked WebSocket
// connections; they must return once their context is done.
func Run(engine *gin.Engine, port string, log *zap.Logger, hooks ...func(context.Context)) error {
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", zap.String("port", port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down", zap.Duration("timeout", shutdownTimeout))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		var hooksDone sync.WaitGroup
		for _, hook := range hooks {
			hooksDone.Go(func() { hook(shutdownCtx) })
		}
		err := srv.Shutdown(shutdownCtx)
		hooksDone.Wait()
		if err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		log.Info("http server stopped")
		return nil
	}
}

// WorkerShutdownTimeout is how long main waits for background workers after
// the HTTP server has stopped. Kubernetes budget: preStop 5 s + HTTP 15 s +
// workers 5 s = 25 s, inside the default 30 s grace period.
const WorkerShutdownTimeout = 5 * time.Second

// Workers tracks background goroutines (match loop, outbox relay, event
// bridge) so main can wait for an in-flight iteration to finish before the
// process exits, instead of the goroutines simply vanishing with it.
type Workers struct {
	wg sync.WaitGroup
}

// Go runs fn in a new goroutine.
func (w *Workers) Go(fn func()) { w.wg.Go(fn) }

// Wait waits for all workers for up to timeout and reports whether they
// all finished in time.
func (w *Workers) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// ShutdownContext returns a context cancelled on SIGINT/SIGTERM, for
// background workers (match loop, event bridge) that must stop with the
// HTTP server.
func ShutdownContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
