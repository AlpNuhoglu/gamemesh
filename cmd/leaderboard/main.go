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
	handler := leaderboard.NewHandler(svc, leaderboard.NewGhostTagStore(rdb))

	// NATS is not a readiness dependency: event publishing here is best effort
	// and never fails a request.
	engine := server.NewEngine(cfg, log, m,
		server.WithReadiness("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() }))
	handler.RegisterRoutes(engine)

	// A deleted account is taken off the boards when its PlayerDeleted event
	// arrives. Accounts are only deleted with the release flag on, so the
	// consumer (a new durable consumer on the player stream) is only started
	// with it.
	ctx, stop := server.ShutdownContext()
	defer stop()
	var workers server.Workers
	if cfg.GameClientEnabled {
		remover := leaderboard.NewPlayerRemover(rdb, log)
		workers.Go(func() {
			if err := events.Consume(ctx, bus, remover.Handle, events.TopicPlayer); err != nil {
				// Not fatal: the boards keep working, and a deleted player
				// stays on them until the consumer is back.
				log.Error("player event consumer stopped", zap.Error(err))
			}
		})
	}

	runErr := server.Run(engine, cfg.HTTPPort, log)
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
