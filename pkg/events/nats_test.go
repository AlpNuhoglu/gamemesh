package events

import (
	"context"
	"fmt"
	"net"
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

// TestNATSPing backs the readiness check: a live connection answers, and a
// lost server is reported within the probe's 1 s budget.
func TestNATSPing(t *testing.T) {
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
	})
	require.NoError(t, err)
	go srv.Start()
	require.True(t, srv.ReadyForConnections(5*time.Second))

	bus, err := NewNATSBus(srv.ClientURL(), "test", 1, nil, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, bus.Ping(ctx))

	srv.Shutdown()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.Error(t, bus.Ping(ctx))
}

// subscribeCounting starts n buses for one service and counts the
// matchmaking events each receives.
func subscribeCounting(ctx context.Context, t *testing.T, url string, n int, opts ...NATSOption) []*atomic.Int64 {
	t.Helper()
	got := make([]*atomic.Int64, n)
	for i := range got {
		got[i] = new(atomic.Int64)
		bus, err := NewNATSBus(url, "websocket", 4, nil, zap.NewNop(), opts...)
		require.NoError(t, err)
		t.Cleanup(func() { _ = bus.Close() })
		c := got[i]
		go func() {
			_ = bus.SubscribeAck(ctx, func(context.Context, Event) error { c.Add(1); return nil }, TopicMatchmaking)
		}()
	}
	time.Sleep(300 * time.Millisecond) // let the consumers attach
	return got
}

func publishN(ctx context.Context, t *testing.T, url string, n int) {
	t.Helper()
	pub, err := NewNATSBus(url, "matchmaking", 1, nil, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pub.Close() })
	for i := 0; i < n; i++ {
		require.NoError(t, pub.Publish(ctx, TopicMatchmaking, Event{Type: TypeMatchFound}))
	}
}

// TestNATSSharedDurableDeliversEachEventOnce pins the default: the instances
// of a service share one durable, a work queue, so each event is processed
// once across them (what the leaderboard and the outbox rely on).
func TestNATSSharedDurableDeliversEachEventOnce(t *testing.T) {
	url := startEmbeddedJetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got := subscribeCounting(ctx, t, url, 2)
	publishN(ctx, t, url, 20)

	require.Eventually(t, func() bool { return got[0].Load()+got[1].Load() == 20 }, 5*time.Second, 50*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // no late duplicates
	assert.Equal(t, int64(20), got[0].Load()+got[1].Load())
}

// TestNATSBroadcastDeliversToEveryInstance is the WebSocket fan-out: every
// replica receives every event. With the shared durable two replicas got 11
// and 9 of 20.
func TestNATSBroadcastDeliversToEveryInstance(t *testing.T) {
	url := startEmbeddedJetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got := subscribeCounting(ctx, t, url, 2, WithBroadcast())
	publishN(ctx, t, url, 20)

	require.Eventually(t, func() bool { return got[0].Load() == 20 && got[1].Load() == 20 }, 5*time.Second, 50*time.Millisecond)
}

// TestNATSBroadcastStartsAtNewEvents: a new replica does not replay the
// stream, which would resend old matches to the players connected to it.
func TestNATSBroadcastStartsAtNewEvents(t *testing.T) {
	url := startEmbeddedJetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	publishN(ctx, t, url, 3) // history
	got := subscribeCounting(ctx, t, url, 1, WithBroadcast())
	publishN(ctx, t, url, 2)

	require.Eventually(t, func() bool { return got[0].Load() == 2 }, 5*time.Second, 50*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int64(2), got[0].Load())
}

// freePort returns a TCP port nothing listens on right now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestNATSConnectWaitsForServer: a service that starts before NATS (no start
// order in Kubernetes) waits for it instead of exiting and crash-looping.
func TestNATSConnectWaitsForServer(t *testing.T) {
	port := freePort(t)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: port, JetStream: true, StoreDir: t.TempDir()})
		if err != nil {
			return
		}
		go srv.Start()
		t.Cleanup(srv.Shutdown)
	}()

	start := time.Now()
	bus, err := NewNATSBus(fmt.Sprintf("nats://127.0.0.1:%d", port), "test", 1, nil, zap.NewNop(), WithConnectWait(10*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	assert.GreaterOrEqual(t, time.Since(start), 1500*time.Millisecond)
}

// TestNATSConnectGivesUp: with no server at all, startup fails after the wait
// instead of hanging.
func TestNATSConnectGivesUp(t *testing.T) {
	start := time.Now()
	_, err := NewNATSBus(fmt.Sprintf("nats://127.0.0.1:%d", freePort(t)), "test", 1, nil, zap.NewNop(), WithConnectWait(time.Second))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreachable after")
	assert.Less(t, time.Since(start), 3*time.Second)
}
