// The leaderboard service maintains global rankings on a Redis sorted set
// and publishes LeaderboardUpdated events on every score change.
package main

import (
	"context"
	"os"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/leaderboard"
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
	cfg := config.Load("leaderboard")
	log := logger.Must(cfg.ServiceName, cfg.Env, logger.SampleConfig{
		Initial:    cfg.LogSampleInitial,
		Thereafter: cfg.LogSampleThereafter,
	})
	defer func() { _ = log.Sync() }()
	// In production, refuse to start with a secret that is unset, short or a
	// known default rather than run with one anyone can read in the repository.
	if err := cfg.Validate(config.NeedRedis); err != nil {
		log.Fatal("refusing to start with weak secrets", zap.Error(err))
	}

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
	bus, err := events.NewBus(events.Config{
		Transport:   cfg.EventBus,
		DurableName: cfg.ServiceName,
		Workers:     cfg.EventWorkers,
	}, rdb, cfg.NATSURL, m, log)
	if err != nil {
		log.Fatal("failed to init event bus", zap.Error(err))
	}
	defer func() { _ = bus.Close() }()
	svc := leaderboard.NewService(leaderboard.NewStore(rdb), bus, m, log)
	handler := leaderboard.NewHandler(svc)

	// NATS is not a readiness dependency: event publishing here is best effort
	// and never fails a request.
	engine := server.NewEngine(cfg, log, m,
		server.WithReadiness("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() }))
	handler.RegisterRoutes(engine)

	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Error("server exited", zap.Error(err))
		return 1
	}
	return 0
}
