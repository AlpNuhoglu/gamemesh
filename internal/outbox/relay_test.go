package outbox

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// fakeBatcher is an in-memory Batcher: it models the outbox table and the
// poll/publish/mark transaction without a database, so the relay's publish and
// retry logic can be tested in isolation.
type fakeBatcher struct {
	mu      sync.Mutex
	rows    []Event           // current PENDING rows, oldest first
	attempt map[uuid.UUID]int // attempt_count per id
}

func newFakeBatcher(rows ...Event) *fakeBatcher {
	return &fakeBatcher{rows: rows, attempt: map[uuid.UUID]int{}}
}

func (f *fakeBatcher) RunBatch(ctx context.Context, limit, maxAttempts int, publish PublishFunc) (polled, deadLettered int, err error) {
	f.mu.Lock()
	batch := f.rows
	if len(batch) > limit {
		batch = batch[:limit]
	}
	// Copy so the publish callback can't mutate our slice mid-flight.
	rows := make([]Event, len(batch))
	copy(rows, batch)
	f.mu.Unlock()

	if len(rows) == 0 {
		return 0, 0, nil
	}

	published, failed, err := publish(ctx, rows)
	if err != nil {
		return len(rows), 0, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	done := map[uuid.UUID]bool{}
	for _, id := range published {
		done[id] = true
	}
	// dead collects failed rows whose bumped attempt_count reached maxAttempts:
	// they leave the PENDING set (dead-lettered) just like the SQL store does.
	dead := map[uuid.UUID]bool{}
	for _, id := range failed {
		f.attempt[id]++
		if maxAttempts > 0 && f.attempt[id] >= maxAttempts {
			dead[id] = true
			deadLettered++
		}
	}
	// Drop published and dead-lettered rows; transiently failed rows stay PENDING.
	var remaining []Event
	for _, r := range f.rows {
		if !done[r.ID] && !dead[r.ID] {
			remaining = append(remaining, r)
		}
	}
	f.rows = remaining
	return len(rows), deadLettered, nil
}

func (f *fakeBatcher) CountPending(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rows)), nil
}

// capturingPublisher records every event handed to it and can be told to fail.
type capturingPublisher struct {
	mu       sync.Mutex
	events   []events.Event
	failNext int // number of upcoming Publish calls that should fail
}

func (p *capturingPublisher) Publish(_ context.Context, _ string, e events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNext > 0 {
		p.failNext--
		return assertErr
	}
	p.events = append(p.events, e)
	return nil
}
func (p *capturingPublisher) Close() error { return nil }
func (p *capturingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

var assertErr = &publishError{}

type publishError struct{}

func (*publishError) Error() string { return "simulated publish failure" }

func pendingRow(t *testing.T, eventType string, payload any) Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return Event{
		ID:        uuid.New(),
		EventType: eventType,
		Topic:     events.TopicPlayer,
		Payload:   raw,
		Carrier:   []byte("{}"),
		Status:    StatusPending,
		CreatedAt: time.Now().UTC(),
	}
}

// Scenario 2: the relay publishes a pending event exactly once and the row is
// removed from the PENDING set (marked PUBLISHED).
func TestRelayPublishesPendingEvent(t *testing.T) {
	row := pendingRow(t, events.TypePlayerRegistered, events.PlayerRegisteredPayload{PlayerID: "p1"})
	fb := newFakeBatcher(row)
	pub := &capturingPublisher{}
	m := metrics.New("test-relay-publish")
	relay := NewRelay(fb, pub, RelayConfig{BatchSize: 10}, m, zap.NewNop())

	n, err := relay.processBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, pub.count(), "event must be published once")

	pending, _ := fb.CountPending(context.Background())
	assert.Equal(t, int64(0), pending, "published row must leave the PENDING set")
}

// Scenario 3: a publish failure leaves the row PENDING with attempt_count bumped;
// a later tick succeeds and publishes it.
func TestRelayRetriesPublishFailure(t *testing.T) {
	row := pendingRow(t, events.TypePlayerRegistered, events.PlayerRegisteredPayload{PlayerID: "p1"})
	fb := newFakeBatcher(row)
	pub := &capturingPublisher{failNext: 1}
	m := metrics.New("test-relay-retry")
	relay := NewRelay(fb, pub, RelayConfig{BatchSize: 10}, m, zap.NewNop())

	// First tick: publish fails, row stays PENDING.
	_, err := relay.processBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, pub.count(), "nothing published on the failing tick")
	pending, _ := fb.CountPending(context.Background())
	assert.Equal(t, int64(1), pending, "failed row must remain PENDING for retry")
	assert.Equal(t, 1, fb.attempt[row.ID], "attempt_count must be incremented")

	// Second tick: publish succeeds.
	_, err = relay.processBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, pub.count(), "event published on the retry tick")
	pending, _ = fb.CountPending(context.Background())
	assert.Equal(t, int64(0), pending)
}

// Scenario 3a: a row that keeps failing is dead-lettered once it reaches
// MaxAttempts — it leaves the PENDING set so it is never retried again, and the
// dead-letter metric is incremented for an operator to alert on.
func TestRelayDeadLettersAfterMaxAttempts(t *testing.T) {
	row := pendingRow(t, events.TypePlayerRegistered, events.PlayerRegisteredPayload{PlayerID: "p1"})
	fb := newFakeBatcher(row)
	pub := &capturingPublisher{failNext: 100} // every publish fails
	m := metrics.New("test-relay-deadletter")
	relay := NewRelay(fb, pub, RelayConfig{BatchSize: 10, MaxAttempts: 3}, m, zap.NewNop())

	// Two failing ticks: row stays PENDING, attempts 1 then 2.
	for i := 1; i <= 2; i++ {
		_, err := relay.processBatch(context.Background())
		require.NoError(t, err)
		pending, _ := fb.CountPending(context.Background())
		assert.Equal(t, int64(1), pending, "row still pending before max attempts")
		assert.Equal(t, i, fb.attempt[row.ID])
	}

	// Third failing tick reaches MaxAttempts: the row is dead-lettered.
	_, err := relay.processBatch(context.Background())
	require.NoError(t, err)
	pending, _ := fb.CountPending(context.Background())
	assert.Equal(t, int64(0), pending, "dead-lettered row must leave the PENDING set")
	assert.Equal(t, 0, pub.count(), "nothing was ever published")

	// A further tick is a no-op: there is nothing left to poll, so the poison row
	// can never wedge a worker again.
	_, err = relay.processBatch(context.Background())
	require.NoError(t, err)
	pending, _ = fb.CountPending(context.Background())
	assert.Equal(t, int64(0), pending)
}

// Scenario 4: duplicate-safe replay. If the process "crashes" after publishing
// but before marking (modeled by re-running the same row), the event is sent
// again — duplicates are possible, and the Event.ID is stable so a consumer can
// dedup on it.
func TestRelayDuplicateSafeReplay(t *testing.T) {
	row := pendingRow(t, events.TypePlayerRegistered, events.PlayerRegisteredPayload{PlayerID: "p1"})
	pub := &capturingPublisher{}
	m := metrics.New("test-relay-dup")

	// Two independent batchers both holding the same row simulate the row never
	// being marked PUBLISHED between two publish cycles (the crash window).
	relay := NewRelay(newFakeBatcher(row), pub, RelayConfig{BatchSize: 10}, m, zap.NewNop())
	_, err := relay.processBatch(context.Background())
	require.NoError(t, err)
	relay2 := NewRelay(newFakeBatcher(row), pub, RelayConfig{BatchSize: 10}, m, zap.NewNop())
	_, err = relay2.processBatch(context.Background())
	require.NoError(t, err)

	require.Equal(t, 2, pub.count(), "the same event is published twice across the crash window")
	assert.Equal(t, pub.events[0].ID, pub.events[1].ID, "stable Event.ID is the consumer dedup key")
}

// Scenario 5: trace propagation through the relay. A carrier captured at write
// time is replayed so the relay's publish span continues the originating trace.
func TestRelayPropagatesTrace(t *testing.T) {
	rec := setupRelayTracing(t)

	// Producer captures its trace context into a carrier (as OutboxPublisher does).
	ctx, producer := otel.Tracer("test").Start(context.Background(), "register")
	wantTrace := producer.SpanContext().TraceID()
	carrier := map[string]string{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
	producer.End()
	carrierJSON, err := json.Marshal(carrier)
	require.NoError(t, err)

	row := pendingRow(t, events.TypePlayerRegistered, events.PlayerRegisteredPayload{PlayerID: "p1"})
	row.Carrier = carrierJSON

	pub := &capturingPublisher{}
	relay := NewRelay(newFakeBatcher(row), pub, RelayConfig{BatchSize: 10}, nil, zap.NewNop())
	_, err = relay.processBatch(context.Background())
	require.NoError(t, err)

	// The republished event carries the original traceparent.
	require.Equal(t, 1, pub.count())
	got := pub.events[0]
	require.Contains(t, got.Carrier, "traceparent")
	rctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier(got.Carrier))
	assert.Equal(t, wantTrace, trace.SpanContextFromContext(rctx).TraceID())

	// And the relay's outbox.publish span is part of the same trace.
	var found bool
	for _, s := range rec.Ended() {
		if s.Name() == "outbox.publish "+events.TopicPlayer {
			found = true
			assert.Equal(t, wantTrace, s.SpanContext().TraceID(), "publish span must continue the originating trace")
		}
	}
	assert.True(t, found, "outbox.publish span must be recorded")
}

func setupRelayTracing(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return rec
}
