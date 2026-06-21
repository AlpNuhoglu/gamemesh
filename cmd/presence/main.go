// The presence service tracks where every player is (OFFLINE, ONLINE,
// IN_QUEUE, IN_MATCH, AWAY) in Redis, refreshed by WS-gateway heartbeats with a
// TTL so presence self-heals after crashes, and publishes presence transitions
// as events for the social layer (friends, parties, invites, notifications).
package main

import (
	"context"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/presence"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/logger"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/server"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

func main() {
	cfg := config.Load("presence")
	log := logger.Must(cfg.ServiceName, cfg.Env)
	defer func() { _ = log.Sync() }()

	shutdownTracing := tracing.MustInit(context.Background(), tracing.Config{
		Enabled:      cfg.OTelEnabled,
		ServiceName:  cfg.OTelServiceName,
		Endpoint:     cfg.OTelEndpoint,
		Env:          cfg.Env,
		Version:      cfg.ServiceVersion,
		Sampler:      cfg.OTelSampler,
		SamplerRatio: cfg.OTelSamplerRatio,
	}, log)
	defer func() { _ = shutdownTracing(context.Background()) }()

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err := tracing.InstrumentRedis(rdb); err != nil {
		log.Fatal("failed to instrument redis", zap.Error(err))
	}

	m := metrics.New(cfg.ServiceName)
	bus, err := events.NewBus(events.Config{
		Transport:   cfg.EventBus,
		DurableName: cfg.ServiceName,
		Workers:     cfg.EventWorkers,
	}, rdb, cfg.NATSURL, m, log)
	if err != nil {
		log.Fatal("failed to init event bus", zap.Error(err))
	}
	defer func() { _ = bus.Close() }()

	repo := presence.NewRepository(rdb, cfg.PresenceTTL)
	svc := presence.NewService(repo, bus, m, log)
	handler := presence.NewHandler(svc)

	engine := server.NewEngine(cfg, log, m)
	handler.RegisterRoutes(engine)

	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
