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

// NewEngine builds a gin engine with the standard middleware chain plus
// /healthz and /metrics.
func NewEngine(cfg *config.Config, log *zap.Logger, m *metrics.Metrics) *gin.Engine {
	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
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
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": cfg.ServiceName})
	})
	r.GET("/metrics", gin.WrapH(m.Handler()))
	return r
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
