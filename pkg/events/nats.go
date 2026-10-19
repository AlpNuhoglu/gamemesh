package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// NATSBus implements Publisher, Subscriber and AckSubscriber over NATS
// JetStream.
//
// Why JetStream over Core NATS (and over the Redis Pub/Sub it replaces):
//   - Durability: events are persisted to a stream, so a consumer that is down
//     when an event is published still receives it on restart. Redis Pub/Sub is
//     fire-and-forget — a missed message is gone.
//   - At-least-once delivery: durable consumers + explicit ACKs guarantee an
//     event is redelivered until processing succeeds.
//   - Replay: a new or reset consumer can re-read the whole stream history
//     (DeliverAll), which is impossible with Pub/Sub.
//   - Outbox-ready: a future Outbox relay reuses Publish unchanged; the durable
//     stream is the reliable hand-off point between DB and consumers.
//
// Redis stays the system of record for sessions, the matchmaking queue, the
// leaderboard sorted set and the room cache — JetStream only replaces the
// transport for inter-service events.
type NATSBus struct {
	conn    *nats.Conn
	js      jetstream.JetStream
	log     *zap.Logger
	m       *metrics.Metrics
	workers int
	durable string // durable consumer name prefix (per consuming service)

	// broadcast gives this instance its own consumer instead of the shared
	// durable, so every instance receives every event (WithBroadcast).
	broadcast bool
	// connectWait bounds how long the first connection is retried.
	connectWait time.Duration

	mu       sync.Mutex
	stop     chan struct{} // closed by Close to trigger early consumer drain
	consumes []jetstream.ConsumeContext
}

// stream describes the JetStream stream backing a topic family. Streams are
// per-domain (not one catch-all) so each domain's retention, limits and replay
// can be tuned independently and one domain's load never affects another.
type stream struct {
	name     string
	subjects []string
	maxAge   time.Duration
}

// streamFor maps a publish topic to the stream that should own it. Topics keep
// their existing values (events.matchmaking / events.leaderboard) so the event
// contract is unchanged; the stream subject is the hierarchical wildcard form
// (events.matchmaking.>) so future event subtypes fit without new streams.
var streamFor = map[string]stream{
	TopicMatchmaking: {
		name:     "MATCHMAKING",
		subjects: []string{TopicMatchmaking, TopicMatchmaking + ".>"},
		maxAge:   24 * time.Hour, // match events are valuable to replay for a day
	},
	TopicLeaderboard: {
		name:     "LEADERBOARD",
		subjects: []string{TopicLeaderboard, TopicLeaderboard + ".>"},
		maxAge:   1 * time.Hour, // score updates are high-volume and disposable
	},
	TopicPlayer: {
		name:     "PLAYER",
		subjects: []string{TopicPlayer, TopicPlayer + ".>"},
		maxAge:   72 * time.Hour, // identity events are low-volume, long replay window
	},
	TopicPresence: {
		name:     "PRESENCE",
		subjects: []string{TopicPresence, TopicPresence + ".>"},
		maxAge:   15 * time.Minute, // high-churn, disposable; Redis is source of truth, not JetStream replay
	},
}

// NATSOption configures a NATSBus.
type NATSOption func(*NATSBus)

// WithBroadcast makes every instance of a service receive every event. By
// default the instances of a service share one durable consumer, which
// JetStream treats as a work queue: each event goes to one instance, right
// for work done once (leaderboard updates, the outbox). A service that fans
// events out to connections it holds itself, like the WebSocket gateway, needs
// all of them: with the shared durable, two replicas got 11 and 9 of 20 events
// and each player missed the events delivered to the other replica.
//
// Each instance then gets its own consumer. It is ephemeral, so instances that
// come and go with rollouts leave nothing behind (the server deletes it after
// broadcastInactiveThreshold without the instance pulling), and it starts at
// new events: replaying the stream to a fresh pod would resend old matches to
// connected players. An ephemeral consumer on a file-stored stream survives a
// NATS server restart (tested); events published while an instance is down
// are not delivered to it, and its clients resync when they reconnect.
func WithBroadcast() NATSOption { return func(b *NATSBus) { b.broadcast = true } }

// WithConnectWait sets how long NewNATSBus retries the first connection
// (default defaultConnectWait).
func WithConnectWait(d time.Duration) NATSOption { return func(b *NATSBus) { b.connectWait = d } }

// defaultConnectWait is how long a service waits for NATS at startup before
// giving up. Kubernetes starts pods in no particular order; without a retry a
// service started before NATS exited and crash-looped until NATS was up. It
// stays inside the startup probe's 60 s window.
const defaultConnectWait = 30 * time.Second

// broadcastInactiveThreshold is how long the server keeps a broadcast
// consumer whose instance stopped pulling. Longer than a reconnect after a
// NATS restart (about 3 s in tests), so a live instance never loses its
// consumer; short enough that a deleted pod's consumer goes away quickly.
const broadcastInactiveThreshold = time.Minute

// NewNATSBus connects to NATS, ensures the streams exist and returns a bus.
// durableName identifies the consuming service (e.g. "ws-bridge") so durable
// consumers survive restarts and resume from the last ACK'd message. workers
// bounds the consume-side concurrency. m may be nil (metrics skipped).
func NewNATSBus(url, durableName string, workers int, m *metrics.Metrics, log *zap.Logger, opts ...NATSOption) (*NATSBus, error) {
	if workers <= 0 {
		workers = 8
	}
	b := &NATSBus{log: log, m: m, workers: workers, durable: durableName, connectWait: defaultConnectWait, stop: make(chan struct{})}
	for _, opt := range opts {
		opt(b)
	}

	conn, err := connectWithRetry(url, durableName, b.connectWait, log)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("jetstream init: %w", err)
	}
	b.conn, b.js = conn, js

	// Ensure every known stream exists. CreateOrUpdateStream is idempotent, so
	// every service can call this safely on boot regardless of start order.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range dedupeStreams() {
		if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
			Name:      s.name,
			Subjects:  s.subjects,
			Retention: jetstream.LimitsPolicy,
			Storage:   jetstream.FileStorage,
			MaxAge:    s.maxAge,
		}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("ensure stream %s: %w", s.name, err)
		}
	}
	return b, nil
}

// connectWithRetry dials NATS with a capped exponential backoff for up to
// maxWait, the same way database.Open waits for Postgres. Once connected the
// client reconnects by itself (MaxReconnects -1); only the first connection
// needs this.
func connectWithRetry(url, name string, maxWait time.Duration, log *zap.Logger) (*nats.Conn, error) {
	deadline := time.Now().Add(maxWait)
	backoff := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		conn, err := nats.Connect(url,
			nats.Name("gamemesh-"+name),
			nats.MaxReconnects(-1), // reconnect forever; JetStream tolerates gaps
			nats.ReconnectWait(time.Second),
		)
		if err == nil {
			return conn, nil
		}
		if time.Now().Add(backoff).After(deadline) {
			return nil, fmt.Errorf("nats connect: unreachable after %d attempts: %w", attempt, err)
		}
		log.Warn("nats not reachable yet, retrying",
			zap.Int("attempt", attempt), zap.Duration("backoff", backoff), zap.Error(err))
		time.Sleep(backoff)
		backoff = min(2*backoff, 5*time.Second)
	}
}

// dedupeStreams returns the unique set of streams (streamFor maps multiple
// topics, but here each topic maps to a distinct stream; dedupe by name guards
// against future many-to-one mappings).
func dedupeStreams() []stream {
	seen := map[string]bool{}
	var out []stream
	for _, s := range streamFor {
		if !seen[s.name] {
			seen[s.name] = true
			out = append(out, s)
		}
	}
	return out
}

// Publish persists the event to its stream. It mirrors RedisBus.Publish: a
// producer span is started and the trace context is injected into the event's
// Carrier so the trace continues at the consumer — the mechanism is identical
// and transport-agnostic, only messaging.system differs.
func (b *NATSBus) Publish(ctx context.Context, topic string, e Event) error {
	ctx, span := tracing.Tracer().Start(ctx, "events.publish "+topic,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			semconv.MessagingSystemKey.String("nats"),
			semconv.MessagingDestinationName(topic),
			attribute.String("messaging.event.type", e.Type),
		),
	)
	defer span.End()

	e.Carrier = tracing.InjectCarrier(ctx, e.Carrier)

	raw, err := json.Marshal(e)
	if err != nil {
		tracing.RecordError(span, err)
		return err
	}

	// Subject = topic.<type> so it falls under the stream's wildcard and lets
	// future per-type consumers filter (e.g. events.matchmaking.MatchFound)
	// without changing the contract.
	subject := topic
	if e.Type != "" {
		subject = topic + "." + e.Type
	}
	_, err = b.js.Publish(ctx, subject, raw)
	tracing.RecordError(span, err)
	if b.m != nil {
		if err != nil {
			b.m.EventsFailedTotal.WithLabelValues(topic, "publish").Inc()
		} else {
			b.m.EventsPublishedTotal.WithLabelValues(topic, e.Type).Inc()
		}
	}
	return err
}

// Subscribe satisfies the legacy channel-based Subscriber for drop-in
// compatibility. It internally uses SubscribeAck and ACKs as soon as the event
// is handed to the channel. Prefer SubscribeAck for true at-least-once: the
// channel model cannot signal processing success back to the transport.
func (b *NATSBus) Subscribe(ctx context.Context, topics ...string) (<-chan Event, error) {
	out := make(chan Event, 256)
	handler := func(_ context.Context, e Event) error {
		select {
		case out <- e:
			return nil // ACK on enqueue — best-effort, like Pub/Sub
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := b.startConsumers(ctx, handler, topics...); err != nil {
		return nil, err
	}
	// Close the channel when ctx ends so range-based consumers terminate.
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

// SubscribeAck delivers each event to handler from a bounded worker pool and
// ACKs only after handler returns nil — genuine "ACK after successful
// processing". On handler error the message is NAK'd with a short delay for
// redelivery (bounded by the consumer's MaxDeliver). It blocks until ctx is
// cancelled, then drains in-flight work and stops the consumers.
func (b *NATSBus) SubscribeAck(ctx context.Context, handler Handler, topics ...string) error {
	if err := b.startConsumers(ctx, handler, topics...); err != nil {
		return err
	}
	b.log.Info("jetstream consumers started",
		zap.Strings("topics", topics),
		zap.Int("workers", b.workers),
		zap.String("durable", b.durable),
		zap.Bool("broadcast", b.broadcast),
	)
	<-ctx.Done()
	return nil
}

// startConsumers creates (or reuses) a durable pull consumer per topic and
// wires a bounded worker pool to process messages. Concurrency model: instead
// of one goroutine per message (unbounded, risks OOM under burst), we run a
// fixed pool of b.workers goroutines reading from an internal channel.
// jetstream.Consume already provides flow control / backpressure by capping the
// number of in-flight (unacked) messages to PullMaxMessages, so the pool plus
// AckWait bounds memory and prevents the consumer from outrunning processing.
func (b *NATSBus) startConsumers(ctx context.Context, handler Handler, topics ...string) error {
	for _, topic := range topics {
		s, ok := streamFor[topic]
		if !ok {
			return fmt.Errorf("no stream configured for topic %q", topic)
		}

		// Durable name = service + stream, so each service has an independent
		// cursor and restarts resume from the last ACK.
		durable := sanitize(b.durable + "-" + strings.ToLower(s.name))
		ccfg := jetstream.ConsumerConfig{
			AckPolicy:     jetstream.AckExplicitPolicy, // manual ACK required
			AckWait:       30 * time.Second,            // redeliver if not ACK'd in time
			MaxDeliver:    5,                           // bounded retries, then give up
			FilterSubject: topic + ".>",
		}
		var cctx jetstream.Consumer
		var err error
		if b.broadcast {
			// This instance's own consumer; see WithBroadcast.
			ccfg.DeliverPolicy = jetstream.DeliverNewPolicy
			ccfg.InactiveThreshold = broadcastInactiveThreshold
			ccfg.Description = durable + " (per-instance fan-out)"
			cctx, err = b.js.CreateConsumer(ctx, s.name, ccfg)
		} else {
			ccfg.Durable = durable
			ccfg.DeliverPolicy = jetstream.DeliverAllPolicy // replay history on first run
			cctx, err = b.js.CreateOrUpdateConsumer(ctx, s.name, ccfg)
		}
		if err != nil {
			return fmt.Errorf("ensure consumer %s: %w", durable, err)
		}

		work := make(chan jetstream.Msg, b.workers)
		var wg sync.WaitGroup
		wg.Add(b.workers)
		for i := 0; i < b.workers; i++ {
			go func() {
				defer wg.Done()
				for msg := range work {
					b.process(ctx, topic, handler, msg)
				}
			}()
		}

		consume, err := cctx.Consume(func(msg jetstream.Msg) {
			select {
			case work <- msg:
			case <-ctx.Done():
				_ = msg.Nak() // shutting down — let it redeliver later
			}
		}, jetstream.PullMaxMessages(b.workers*2)) // backpressure: cap in-flight
		if err != nil {
			close(work)
			return fmt.Errorf("consume %s: %w", durable, err)
		}

		// Graceful shutdown: when the subscribe ctx ends OR Close is called,
		// stop pulling, drain the pool, then stop the consume context.
		go func() {
			select {
			case <-ctx.Done():
			case <-b.stop:
			}
			consume.Stop() // stop delivering new messages
			close(work)    // let workers drain remaining buffered messages
			wg.Wait()
		}()

		b.mu.Lock()
		b.consumes = append(b.consumes, consume)
		b.mu.Unlock()
	}
	return nil
}

// process runs the handler for one message and translates its result into an
// ACK (success) or NAK-with-delay (failure → redelivery), recording metrics and
// a consumer span. This is the single point where at-least-once is enforced:
// the message is only removed from the stream's pending set after handler
// returns nil.
func (b *NATSBus) process(ctx context.Context, topic string, handler Handler, msg jetstream.Msg) {
	var e Event
	if err := json.Unmarshal(msg.Data(), &e); err != nil {
		b.log.Warn("dropping malformed event", zap.Error(err), zap.String("subject", msg.Subject()))
		if b.m != nil {
			b.m.EventsFailedTotal.WithLabelValues(topic, "decode").Inc()
		}
		_ = msg.Term() // poison message — never redeliver
		return
	}

	// Continue the producer's trace across the async boundary. The context is
	// extracted UNCONDITIONALLY (this never opens a span), so trace propagation
	// is preserved even when the consumer span below is sampled out for a
	// high-volume event type. StartConsumerSpan then either opens a recording
	// span or, for a sampled-out high-volume event, returns the propagating
	// context + (non-recording) span — keeping the trace chain intact while
	// dropping the exporter cost. See tracing.StartConsumerSpan.
	hctx := tracing.ResumeFromCarrier(ctx, e.Carrier)
	hctx, span := tracing.StartConsumerSpan(hctx, "events.consume "+topic, e.Type,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			semconv.MessagingSystemKey.String("nats"),
			attribute.String("messaging.event.type", e.Type),
		),
	)
	defer span.End()

	start := time.Now()
	err := handler(hctx, e)
	if b.m != nil {
		b.m.EventProcessingDuration.WithLabelValues(topic, e.Type).Observe(time.Since(start).Seconds())
	}

	if err != nil {
		tracing.RecordError(span, err)
		b.log.Warn("event handler failed, will redeliver",
			zap.Error(err), zap.String("type", e.Type), zap.String("id", e.ID))
		if b.m != nil {
			b.m.EventsFailedTotal.WithLabelValues(topic, "handle").Inc()
		}
		// NAK with backoff; JetStream redelivers up to MaxDeliver.
		_ = msg.NakWithDelay(2 * time.Second)
		return
	}

	if ackErr := msg.Ack(); ackErr != nil {
		// ACK failed (e.g. connection blip). The message will redeliver on
		// AckWait expiry — at-least-once holds, handler must be idempotent.
		b.log.Warn("ack failed; message will redeliver", zap.Error(ackErr), zap.String("id", e.ID))
		tracing.RecordError(span, ackErr)
		return
	}
	if b.m != nil {
		b.m.EventsConsumedTotal.WithLabelValues(topic, e.Type).Inc()
	}
}

// Ping reports whether the NATS connection is up. The PING/PONG round trip
// also catches a connection that died without the client noticing yet.
func (b *NATSBus) Ping(ctx context.Context) error {
	if !b.conn.IsConnected() {
		return fmt.Errorf("nats: %s", b.conn.Status())
	}
	return b.conn.FlushWithContext(ctx)
}

// Close stops all consumers and drains the NATS connection. It is safe to call
// once; the stop channel signals each consumer's drain goroutine to wind down.
func (b *NATSBus) Close() error {
	b.mu.Lock()
	select {
	case <-b.stop: // already closed
	default:
		close(b.stop)
	}
	for _, c := range b.consumes {
		c.Stop()
	}
	b.consumes = nil
	b.mu.Unlock()

	// Drain flushes pending publishes and waits for in-flight callbacks.
	if err := b.conn.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		return err
	}
	return nil
}

// sanitize makes a string safe for use as a NATS durable name (no dots/spaces).
func sanitize(s string) string {
	return strings.NewReplacer(".", "_", " ", "_", "*", "_", ">", "_").Replace(s)
}
