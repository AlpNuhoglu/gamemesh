package events

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// setupTracing installs a recording tracer provider and the W3C propagator for
// the duration of a test, restoring the globals afterwards.
func setupTracing(t *testing.T) *tracetest.SpanRecorder {
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

// TestPublishInjectsTraceContext asserts the publish path embeds the active
// trace into the event Carrier — the producer half of the async boundary.
func TestPublishInjectsTraceContext(t *testing.T) {
	setupTracing(t)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	bus := NewRedisBus(rdb, zap.NewNop())
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := bus.Subscribe(ctx, TopicLeaderboard)
	require.NoError(t, err)

	// Publish inside an active parent span.
	ctx, parent := otel.Tracer("test").Start(ctx, "parent")
	wantTrace := parent.SpanContext().TraceID()

	sent, err := New(TypeLeaderboardUpdated, LeaderboardUpdatedPayload{PlayerID: "p1", Score: 1, Rank: 1})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, TopicLeaderboard, sent))
	parent.End()

	select {
	case got := <-ch:
		// The carrier survived the Redis round-trip and holds the traceparent.
		require.NotNil(t, got.Carrier, "carrier must be populated by Publish")
		require.Contains(t, got.Carrier, "traceparent")

		// Extracting it yields the SAME trace id — the boundary is preserved.
		rctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier(got.Carrier))
		gotTrace := trace.SpanContextFromContext(rctx).TraceID()
		assert.Equal(t, wantTrace, gotTrace, "trace id must survive publish->subscribe")
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

// TestReceiveSpanContinuesTrace asserts the consumer half: ReceiveSpan extracts
// the carrier and starts a span that is a child of the producing trace.
func TestReceiveSpanContinuesTrace(t *testing.T) {
	rec := setupTracing(t)

	// Simulate a producer: build a carrier from an active span.
	ctx, producer := otel.Tracer("test").Start(context.Background(), "producer")
	wantTrace := producer.SpanContext().TraceID()
	carrier := map[string]string{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
	producer.End()

	// Consumer side.
	e := Event{Type: TypeMatchFound, Carrier: carrier}
	_, span := ReceiveSpan(context.Background(), e, "ws.dispatch")
	span.End()

	// The dispatch span shares the producer's trace id and is SpanKindConsumer.
	var dispatch sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "ws.dispatch" {
			dispatch = s
		}
	}
	require.NotNil(t, dispatch, "ws.dispatch span must be recorded")
	assert.Equal(t, wantTrace, dispatch.SpanContext().TraceID(), "consumer span must continue the producer trace")
	assert.Equal(t, trace.SpanKindConsumer, dispatch.SpanKind())
}
