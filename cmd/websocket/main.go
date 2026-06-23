// The WebSocket gateway holds client connections, manages room
// subscriptions and pushes backend events (MatchFound, LeaderboardUpdated)
// to clients in real time.
package main

import (
	"context"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/presence"
	"github.com/alpnuhoglu/gamemesh/internal/wsgateway"
	"github.com/alpnuhoglu/gamemesh/pkg/auth"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/logger"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/server"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

func main() {
	cfg := config.Load("websocket")
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

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err := tracing.InstrumentRedis(rdb); err != nil {
		log.Fatal("failed to instrument redis", zap.Error(err))
	}

	m := metrics.New(cfg.ServiceName)
	// Feed presence over HTTP. The notifier is the only coupling between the WS
	// gateway and the Presence Service; if the Presence Service is down, calls
	// fail softly and presence self-heals from later heartbeats / TTL.
	notifier := presence.NewHTTPNotifier(cfg.PresenceServiceURL, cfg.PresenceHeartbeatInterval)
	hub := wsgateway.NewHub(log, m, notifier)
	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTExpiry, cfg.JWTIssuer)
	handler := wsgateway.NewHandler(hub, tokens, cfg.AllowedOrigins, log)

	ctx, stop := server.ShutdownContext()
	defer stop()

	// Refresh presence TTL for every locally-connected player on a ticker.
	go hub.RunHeartbeat(ctx, cfg.PresenceHeartbeatInterval)
	bus, err := events.NewBus(events.Config{
		Transport:   cfg.EventBus,
		DurableName: cfg.ServiceName,
		Workers:     cfg.EventWorkers,
	}, rdb, cfg.NATSURL, m, log)
	if err != nil {
		log.Fatal("failed to init event bus", zap.Error(err))
	}
	defer func() { _ = bus.Close() }()
	bridge := wsgateway.NewBridge(hub, bus, log)
	go func() {
		if err := bridge.Run(ctx); err != nil {
			log.Fatal("event bridge failed", zap.Error(err))
		}
	}()

	engine := server.NewEngine(cfg, log, m)
	handler.RegisterRoutes(engine)

	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
