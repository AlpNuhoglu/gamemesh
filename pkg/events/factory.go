package events

import (
	"fmt"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// Bus is the union of the transport capabilities. Every concrete transport
// (RedisBus, NATSBus) satisfies Publisher + Subscriber; NATSBus additionally
// satisfies AckSubscriber. Returning the union lets callers type-assert for the
// richer contract (the WS bridge does this) while still depending only on the
// narrow interfaces for the common path.
type Bus interface {
	Publisher
	Subscriber
}

// Config selects and configures the event transport. It deliberately carries
// the few transport-agnostic knobs the factory needs; services never see it —
// they receive a Publisher/Subscriber.
type Config struct {
	// Transport is "redis" or "nats". Unknown values are an error so a typo in
	// EVENT_BUS fails loudly at startup rather than silently picking a default.
	Transport string

	// DurableName identifies the consuming service for NATS durable consumers
	// (ignored by Redis). E.g. "ws-bridge".
	DurableName string

	// Workers bounds NATS consume-side concurrency (ignored by Redis).
	Workers int
}

// NewBus constructs the configured transport. It is the single place the
// transport switch lives, so no cmd/* entrypoint duplicates it and adding a
// future transport (Kafka, gRPC) touches one file. rdb is reused for the Redis
// transport; natsURL is dialed for the NATS transport.
//
// This is the seam that makes the migration a configuration change, not a code
// change: business services keep injecting the returned Publisher/Subscriber.
func NewBus(cfg Config, rdb *redis.Client, natsURL string, m *metrics.Metrics, log *zap.Logger) (Bus, error) {
	switch cfg.Transport {
	case "nats":
		return NewNATSBus(natsURL, cfg.DurableName, cfg.Workers, m, log)
	case "redis", "":
		return NewRedisBus(rdb, log, m), nil
	default:
		return nil, fmt.Errorf("unknown EVENT_BUS %q (want \"redis\" or \"nats\")", cfg.Transport)
	}
}
