package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// startEmbeddedJetStream boots an in-process NATS server with JetStream enabled
// so these tests need no docker. It returns the client URL.
func startEmbeddedJetStream(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // random free port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	srv, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded NATS server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// TestNATSPublishConsumeAck is the happy path: an event published to JetStream
// is delivered to the handler, processed, and acknowledged exactly with the
// trace context preserved across the async boundary.
func TestNATSPublishConsumeAck(t *testing.T) {
	setupTracing(t)
	url := startEmbeddedJetStream(t)

	bus, err := NewNATSBus(url, "test", 4, nil, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	gotTrace := make(chan trace.TraceID, 1)
	handler := func(hctx context.Context, _ Event) error {
		gotTrace <- trace.SpanContextFromContext(hctx).TraceID()
		return nil
	}
	go func() { _ = bus.SubscribeAck(ctx, handler, TopicMatchmaking) }()
	// Give the consumer a moment to attach.
	time.Sleep(300 * time.Millisecond)

	// Publish inside an active parent span so we can assert trace continuity.
	pctx, parent := otel.Tracer("test").Start(ctx, "parent")
	want := parent.SpanContext().TraceID()
	e, err := New(TypeMatchFound, MatchFoundPayload{RoomID: "r1", Players: []string{"p1", "p2"}})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(pctx, TopicMatchmaking, e))
	parent.End()

	select {
	case got := <-gotTrace:
		assert.Equal(t, want, got, "consumer handler must run under the producer's trace")
	case <-ctx.Done():
		t.Fatal("timed out waiting for event to be consumed")
	}
}

// TestNATSRedeliversOnHandlerError asserts at-least-once: a handler that fails
// the first time causes a NAK and the event is redelivered until it succeeds.
func TestNATSRedeliversOnHandlerError(t *testing.T) {
	setupTracing(t)
	url := startEmbeddedJetStream(t)

	bus, err := NewNATSBus(url, "retry", 1, nil, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var attempts int32
	done := make(chan struct{})
	handler := func(_ context.Context, _ Event) error {
		if atomic.AddInt32(&attempts, 1) < 2 {
			return assert.AnError // fail first delivery -> NAK -> redeliver
		}
		close(done)
		return nil
	}
	go func() { _ = bus.SubscribeAck(ctx, handler, TopicLeaderboard) }()
	time.Sleep(300 * time.Millisecond)

	e, err := New(TypeLeaderboardUpdated, LeaderboardUpdatedPayload{PlayerID: "p1", Score: 9, Rank: 1})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, TopicLeaderboard, e))

	select {
	case <-done:
		assert.GreaterOrEqual(t, atomic.LoadInt32(&attempts), int32(2), "event must be redelivered after a failed handler")
	case <-ctx.Done():
		t.Fatal("timed out waiting for redelivery")
	}
}

// TestNATSReplay asserts replay: an event published before any consumer exists
// is still delivered, because JetStream persists it (DeliverAll). This is the
// capability Redis Pub/Sub lacks.
func TestNATSReplay(t *testing.T) {
	setupTracing(t)
	url := startEmbeddedJetStream(t)

	bus, err := NewNATSBus(url, "replay", 1, nil, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Publish first, with NO consumer attached yet.
	e, err := New(TypeMatchFound, MatchFoundPayload{RoomID: "r9", Players: []string{"p1"}})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, TopicMatchmaking, e))

	// Now attach a fresh consumer; it must still receive the earlier event.
	got := make(chan string, 1)
	handler := func(_ context.Context, ev Event) error {
		got <- ev.ID
		return nil
	}
	go func() { _ = bus.SubscribeAck(ctx, handler, TopicMatchmaking) }()

	select {
	case id := <-got:
		assert.Equal(t, e.ID, id, "consumer must replay an event published before it existed")
	case <-ctx.Done():
		t.Fatal("timed out waiting for replayed event")
	}
}
