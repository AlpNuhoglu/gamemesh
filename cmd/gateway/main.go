// The API gateway is the single public entry point: it terminates JWT auth,
// applies per-IP rate limiting and proxies to internal services.
package main

import (
	"context"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/gateway"
	"github.com/alpnuhoglu/gamemesh/internal/player"
	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/logger"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/middleware"
	"github.com/alpnuhoglu/gamemesh/pkg/server"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

func main() {
	cfg := config.Load("gateway")
	log := logger.Must(cfg.ServiceName, cfg.Env, logger.SampleConfig{
		Initial:    cfg.LogSampleInitial,
		Thereafter: cfg.LogSampleThereafter,
	})
	defer func() { _ = log.Sync() }()

	shutdownTracing := tracing.MustInit(context.Background(), tracing.Config{
		Enabled:          cfg.OTelEnabled,
		ServiceName:      cfg.OTelServiceName,
		Endpoint:         cfg.OTelEndpoint,
		Env:              cfg.Env,
		Version:          cfg.ServiceVersion,
		Sampler:          cfg.OTelSampler,
		SamplerRatio:     cfg.OTelSamplerRatio,
		HighVolumeEvents: cfg.TraceHighVolumeEvents,
		HighVolumeRatio:  cfg.TraceHighVolumeSampleRatio,
	}, log)
	defer func() { _ = shutdownTracing(context.Background()) }()

	if cfg.Env == "production" && cfg.JWTSecret == "insecure-dev-secret-do-not-use-in-prod" {
		log.Fatal("JWT_SECRET must be set in production")
	}

	m := metrics.New(cfg.ServiceName)
	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTExpiry, cfg.JWTIssuer)

	// The gateway is the boundary node that enforces JWT revocation (logout) so
	// a revoked token is rejected here, before any upstream spends CPU on it. It
	// reads the same cluster Redis the player service writes sessions to; a
	// short-TTL in-process cache keeps that check off the Redis hot path.
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err := tracing.InstrumentRedis(rdb); err != nil {
		log.Fatal("failed to instrument redis", zap.Error(err))
	}
	var sessions middleware.SessionChecker
	store := player.NewSessionStore(rdb)
	if cfg.SessionCacheEnabled {
		store = player.NewCachedSessionStore(store, cfg.SessionCacheTTL)
	}
	sessions = store

	engine := server.NewEngine(cfg, log, m)
	engine.Use(middleware.RateLimit(cfg.RateLimitRPS, cfg.RateLimitBurst))
	gateway.RegisterRoutes(engine, cfg, tokens, sessions, log)

	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
