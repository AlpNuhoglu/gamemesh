package outbox

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Batcher is the storage seam the relay depends on. *Store implements it against
// Postgres; tests substitute an in-memory fake so the relay's publish/retry
// logic is exercised without a database.
type Batcher interface {
	RunBatch(ctx context.Context, limit int, publish PublishFunc) (int, error)
	CountPending(ctx context.Context) (int64, error)
}

// RelayConfig tunes the relay loop.
type RelayConfig struct {
	BatchSize    int           // rows fetched per poll
	PollInterval time.Duration // delay between polls when idle
	Workers      int           // bounded publish concurrency per batch
}

func (c RelayConfig) withDefaults() RelayConfig {
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	return c
}

// Relay polls the outbox and publishes committed rows to the event bus. It is
// the only component that talks to NATS for outbox events; the producing service
// only ever writes to the table. Running it as a separate process lets it scale
// and restart independently of the business services.
type Relay struct {
	store Batcher
	bus   events.Publisher
	cfg   RelayConfig
	m     *metrics.Metrics
	log   *zap.Logger
}

// NewRelay wires a relay. m may be nil (metrics skipped).
func NewRelay(store Batcher, bus events.Publisher, cfg RelayConfig, m *metrics.Metrics, log *zap.Logger) *Relay {
	return &Relay{store: store, bus: bus, cfg: cfg.withDefaults(), m: m, log: log}
}

// Run polls until ctx is cancelled, draining the in-flight batch before
// returning. It loops back-to-back while a poll returns a full batch (catching
// up a backlog) and otherwise sleeps for PollInterval.
func (r *Relay) Run(ctx context.Context) error {
	r.log.Info("outbox relay started",
		zap.Int("batch_size", r.cfg.BatchSize),
		zap.Duration("poll_interval", r.cfg.PollInterval),
		zap.Int("workers", r.cfg.Workers))

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Info("outbox relay stopping")
			return nil
		case <-timer.C:
		}

		n, err := r.processBatch(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Warn("outbox poll failed", zap.Error(err))
		}
		r.updatePendingGauge(ctx)

		// Busy-loop while draining a backlog; otherwise wait a tick.
		if n >= r.cfg.BatchSize {
			timer.Reset(0)
		} else {
			timer.Reset(r.cfg.PollInterval)
		}
	}
}

// processBatch runs one poll → publish → mark cycle inside a single DB
// transaction (owned by the store). The PENDING rows are locked with
// FOR UPDATE SKIP LOCKED for the duration so concurrent relay replicas never
// touch the same rows. Successfully published rows are marked PUBLISHED; failed
// ones get attempt_count bumped and stay PENDING for the next cycle (retry).
// Returns the number of rows polled.
func (r *Relay) processBatch(ctx context.Context) (int, error) {
	pollCtx, span := tracing.Tracer().Start(ctx, "outbox.poll")
	defer span.End()

	polled, err := r.store.RunBatch(pollCtx, r.cfg.BatchSize, func(ctx context.Context, rows []Event) ([]uuid.UUID, []uuid.UUID, error) {
		published, failed := r.publishAll(ctx, rows)
		return published, failed, nil
	})
	tracing.RecordError(span, err)
	return polled, err
}

// publishAll fans the batch out across a bounded worker pool (NOT one goroutine
// per event) and returns the IDs that published successfully and those that
// failed.
func (r *Relay) publishAll(ctx context.Context, rows []Event) (published, failed []uuid.UUID) {
	type result struct {
		id  uuid.UUID
		err error
	}

	jobs := make(chan Event)
	results := make(chan result)

	var wg sync.WaitGroup
	workers := r.cfg.Workers
	if workers > len(rows) {
		workers = len(rows)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range jobs {
				results <- result{id: row.ID, err: r.publishOne(ctx, row)}
			}
		}()
	}

	go func() {
		for _, row := range rows {
			jobs <- row
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		if res.err != nil {
			failed = append(failed, res.id)
		} else {
			published = append(published, res.id)
		}
	}
	return published, failed
}

// publishOne reconstructs the events.Event (including the stored trace carrier)
// and publishes it. The carrier is extracted so the relay's publish span — and
// the downstream NATS producer span — link back to the originating request's
// trace even though publication happens later in this separate process.
func (r *Relay) publishOne(ctx context.Context, row Event) error {
	carrier := map[string]string{}
	if len(row.Carrier) > 0 {
		_ = json.Unmarshal(row.Carrier, &carrier)
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))

	ctx, span := tracing.Tracer().Start(ctx, "outbox.publish "+row.Topic)
	defer span.End()

	e := events.Event{
		ID:        row.ID.String(),
		Type:      row.EventType,
		Timestamp: row.CreatedAt,
		Payload:   row.Payload,
		Carrier:   carrier,
	}

	start := time.Now()
	err := r.bus.Publish(ctx, row.Topic, e)
	if r.m != nil {
		r.m.OutboxPublishDuration.Observe(time.Since(start).Seconds())
	}
	if err != nil {
		tracing.RecordError(span, err)
		if r.m != nil {
			r.m.OutboxPublishFailuresTotal.Inc()
		}
		r.log.Warn("outbox publish failed; will retry",
			zap.String("event_id", row.ID.String()),
			zap.String("event_type", row.EventType),
			zap.Int("attempt", row.AttemptCount+1),
			zap.Error(err))
		return err
	}
	if r.m != nil {
		r.m.OutboxEventsPublishedTotal.Inc()
	}
	return nil
}

func (r *Relay) updatePendingGauge(ctx context.Context) {
	if r.m == nil {
		return
	}
	n, err := r.store.CountPending(ctx)
	if err != nil {
		return
	}
	r.m.OutboxEventsPending.Set(float64(n))
}
