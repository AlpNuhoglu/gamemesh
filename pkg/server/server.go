// Package server provides shared HTTP bootstrap: the base middleware chain,
// health/metrics endpoints and graceful shutdown — identical across all five
// services, so it lives in one place.
package server

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
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
		// status and latency.
		otelgin.Middleware(cfg.OTelServiceName),
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

// Run serves the engine until SIGINT/SIGTERM, then drains in-flight requests
// for up to 15 seconds — required for zero-downtime rolling updates in K8s.
func Run(engine *gin.Engine, port string, log *zap.Logger) error {
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
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ShutdownContext returns a context cancelled on SIGINT/SIGTERM, for
// background workers (match loop, event bridge) that must stop with the
// HTTP server.
func ShutdownContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
