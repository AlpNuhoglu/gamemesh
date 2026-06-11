// The API gateway is the single public entry point: it terminates JWT auth,
// applies per-IP rate limiting and proxies to internal services.
package main

import (
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/gateway"
	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/logger"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
	"github.com/alpnuhoglu/gamemesh/pkg/server"
)

func main() {
	cfg := config.Load("gateway")
	log := logger.Must(cfg.ServiceName, cfg.Env)
	defer func() { _ = log.Sync() }()

	if cfg.Env == "production" && cfg.JWTSecret == "insecure-dev-secret-do-not-use-in-prod" {
		log.Fatal("JWT_SECRET must be set in production")
	}

	m := metrics.New(cfg.ServiceName)
	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTExpiry, cfg.JWTIssuer)

	engine := server.NewEngine(cfg, log, m)
	engine.Use(middleware.RateLimit(cfg.RateLimitRPS, cfg.RateLimitBurst))
	gateway.RegisterRoutes(engine, cfg, tokens, log)

	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
