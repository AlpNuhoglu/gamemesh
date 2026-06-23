// The outbox-relay service is the publish side of the transactional outbox. It
// polls outbox_events (written by the player service in the same transaction as
// the business state) and publishes committed rows to NATS JetStream. Running it
// as a dedicated process keeps the reliability layer decoupled from the business
// services: it scales, restarts and fails independently, and the player service
// never needs a NATS connection.
package main

import (
	"context"

	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/alpnuhoglu/gamemesh/internal/outbox"
	"github.com/alpnuhoglu/gamemesh/pkg/config"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/logger"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/server"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

func main() {
	cfg := config.Load("outbox-relay")
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

	db, err := gorm.Open(postgres.Open(cfg.PostgresDSN), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		log.Fatal("failed to connect to postgres", zap.Error(err))
	}
	if err := tracing.InstrumentGorm(db); err != nil {
		log.Fatal("failed to instrument gorm", zap.Error(err))
	}

	m := metrics.New(cfg.ServiceName)

	// The relay publishes to NATS; it never falls back to Redis Pub/Sub because
	// outbox durability would be pointless behind a fire-and-forget transport.
	bus, err := events.NewBus(events.Config{
		Transport:   "nats",
		DurableName: cfg.ServiceName,
		Workers:     cfg.EventWorkers,
	}, nil, cfg.NATSURL, m, log)
	if err != nil {
		log.Fatal("failed to init event bus", zap.Error(err))
	}
	defer func() { _ = bus.Close() }()

	store := outbox.NewStore(db)
	relay := outbox.NewRelay(store, bus, outbox.RelayConfig{
		BatchSize:    cfg.OutboxBatchSize,
		PollInterval: cfg.OutboxPollInterval,
		Workers:      cfg.OutboxWorkers,
		MaxAttempts:  cfg.OutboxMaxAttempts,
	}, m, log)

	ctx, stop := server.ShutdownContext()
	defer stop()

	if !cfg.OutboxEnabled {
		log.Warn("OUTBOX_ENABLED=false; relay idle, serving metrics only")
	} else {
		go func() {
			if err := relay.Run(ctx); err != nil {
				log.Fatal("outbox relay failed", zap.Error(err))
			}
		}()
	}

	// Serve /metrics (and /healthz via the shared engine) so Prometheus can scrape
	// the relay and Compose/K8s health checks work like every other service.
	engine := server.NewEngine(cfg, log, m)
	if err := server.Run(engine, cfg.HTTPPort, log); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}
