// The matchmaking service queues players in Redis and pairs them by rank
// every MATCH_INTERVAL_SECONDS, publishing MatchFound events.
package main

import (
	"context"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/matchmaking"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/logger"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/server"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

func main() {
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
	svc := matchmaking.NewService(
		matchmaking.NewQueue(rdb),
		matchmaking.NewRoomStore(rdb, cfg.RoomTTL),
		bus, m, log,
		matchmaking.Config{
			RankWindow:  cfg.MatchRankWindow,
			BatchSize:   cfg.MatchBatchSize,
			MaxQueueAge: cfg.MatchMaxQueueAge,
		},
	)
	handler := matchmaking.NewHandler(svc)

	ctx, stop := server.ShutdownContext()
	defer stop()
	go svc.RunMatchLoop(ctx, cfg.MatchInterval)

	engine := server.NewEngine(cfg, log, m)
	handler.RegisterRoutes(engine)

	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
