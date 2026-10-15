// The matchmaking service queues players in Redis and pairs them by rank
// every MATCH_INTERVAL_SECONDS, publishing MatchFound events.
package main

import (
	"context"
	"os"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/matchmaking"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/health"
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
	cfg := config.Load("matchmaking")
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
		PoolSize: cfg.RedisPoolSize,
	})
	if err := tracing.InstrumentRedis(rdb); err != nil {
		log.Fatal("failed to instrument redis", zap.Error(err))
	}
	log.Info("redis pool configured", zap.Int("pool_size", cfg.RedisPoolSize))

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
	loopBeat := health.NewHeartbeat()
	svc := matchmaking.NewService(
		matchmaking.NewQueue(rdb),
		matchmaking.NewRoomStore(rdb, cfg.RoomTTL),
		bus, m, log,
		matchmaking.Config{
			RankWindow:  cfg.MatchRankWindow,
			BatchSize:   cfg.MatchBatchSize,
			MaxQueueAge: cfg.MatchMaxQueueAge,
			Heartbeat:   loopBeat,
		},
	)
	handler := matchmaking.NewHandler(svc)

	ctx, stop := server.ShutdownContext()
	defer stop()
	var workers server.Workers
	workers.Go(func() { svc.RunMatchLoop(ctx, cfg.MatchInterval) })

	// NATS is not a readiness dependency: event publishing here is best effort
	// and never fails a request.
	engine := server.NewEngine(cfg, log, m,
		server.WithReadiness("redis", func(ctx context.Context) error { return rdb.Ping(ctx).Err() }),
		server.WithLiveness("match-loop", loopBeat.Check(health.StallAfter(cfg.MatchInterval))))
	handler.RegisterRoutes(engine)

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
