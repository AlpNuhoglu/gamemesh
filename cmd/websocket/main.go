// The WebSocket gateway holds client connections, manages room
// subscriptions and pushes backend events (MatchFound, LeaderboardUpdated)
// to clients in real time.
package main

import (
	"context"
	"os"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/player"
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

func main() { os.Exit(run()) }

// run holds main's body so its deferred cleanup (closing the event bus,
// flushing traces, syncing the log) runs before the process exits, which
// os.Exit and log.Fatal would skip.
func run() int {
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
		PoolSize: cfg.RedisPoolSize, // 0 = go-redis default (10 per CPU)
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
	// Sessions are checked straight against Redis, uncached: once per connection.
	handler := wsgateway.NewHandler(hub, tokens, player.NewSessionStore(rdb), cfg.AllowedOrigins, log)

	ctx, stop := server.ShutdownContext()
	defer stop()
	var workers server.Workers

	// Refresh presence TTL for every locally-connected player on a ticker.
	workers.Go(func() { hub.RunHeartbeat(ctx, cfg.PresenceHeartbeatInterval) })
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
	workers.Go(func() {
		if err := bridge.Run(ctx); err != nil {
			log.Fatal("event bridge failed", zap.Error(err))
		}
	})

	// Delivering events is this service's job, so the bus is a readiness
	// dependency, and so is Redis, which every handshake's session check needs.
	// Presence is another application service and never is.
	engine := server.NewEngine(cfg, log, m,
		server.WithReadiness(cfg.EventBus, bus.Ping),
		server.WithReadiness("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() }))
	handler.RegisterRoutes(engine)

	runErr := server.Run(engine, cfg.HTTPPort, log, hub.CloseAll)
	stop() // also stops the workers if Run failed without a signal
	if workers.Wait(server.WorkerShutdownTimeout) {
		log.Info("background workers stopped")
	} else {
		log.Warn("background workers still running at exit", zap.Duration("waited", server.WorkerShutdownTimeout))
	}
	if runErr != nil {
		log.Error("server exited", zap.Error(runErr))
		return 1
	}
	return 0
}
