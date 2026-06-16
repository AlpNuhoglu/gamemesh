package events

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// RedisBus implements Publisher and Subscriber over Redis Pub/Sub.
//
// Why Redis Pub/Sub: it is already in the stack (leaderboard, matchmaking
// queue), has sub-millisecond fan-out, and fits fire-and-forget realtime
// notifications where a missed message is acceptable (the next leaderboard
// poll or match tick repairs state). For durable delivery guarantees the
// Publisher interface would be re-implemented on NATS JetStream or Kafka.
type RedisBus struct {
	client *redis.Client
	log    *zap.Logger
	subs   []*redis.PubSub
}

// NewRedisBus wraps an existing Redis client.
func NewRedisBus(client *redis.Client, log *zap.Logger) *RedisBus {
	return &RedisBus{client: client, log: log}
}

// Publish marshals and publishes the event to a topic. It creates a producer
// span and injects the current trace context into the event's Carrier so the
// trace continues at the subscriber (see ReceiveSpan).
func (b *RedisBus) Publish(ctx context.Context, topic string, e Event) error {
	ctx, span := tracing.Tracer().Start(ctx, "events.publish "+topic,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			semconv.MessagingSystemKey.String("redis"),
			semconv.MessagingDestinationName(topic),
			attribute.String("messaging.event.type", e.Type),
		),
	)
	defer span.End()

	// Inject W3C trace context into the carrier (transport-agnostic).
	if e.Carrier == nil {
		e.Carrier = make(map[string]string)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(e.Carrier))

	raw, err := json.Marshal(e)
	if err != nil {
		tracing.RecordError(span, err)
		return err
	}
	err = b.client.Publish(ctx, topic, raw).Err()
	tracing.RecordError(span, err)
	return err
}

// ReceiveSpan extracts the trace context embedded in an event's Carrier and
// starts a consumer span linked to the producing trace. Consumers (e.g. the
// WebSocket bridge) call this so the async hop appears as one connected
// distributed trace. Returns the new context and span; the caller must End the
// span. Transport-agnostic: relies only on the OTel propagator and the Carrier.
func ReceiveSpan(ctx context.Context, e Event, name string) (context.Context, trace.Span) {
	if e.Carrier != nil {
		ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(e.Carrier))
	}
	return tracing.Tracer().Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			semconv.MessagingSystemKey.String("redis"),
			attribute.String("messaging.event.type", e.Type),
		),
	)
}

// Subscribe listens on the given topics and forwards decoded events. The
// returned channel closes when ctx is cancelled or the bus is closed.
func (b *RedisBus) Subscribe(ctx context.Context, topics ...string) (<-chan Event, error) {
	ps := b.client.Subscribe(ctx, topics...)
	// Force the subscription to be established before returning so callers
	// never miss events published right after Subscribe.
	if _, err := ps.Receive(ctx); err != nil {
		return nil, err
	}
	b.subs = append(b.subs, ps)

	out := make(chan Event, 256)
	go func() {
		defer close(out)
		for msg := range ps.Channel() {
			var e Event
			if err := json.Unmarshal([]byte(msg.Payload), &e); err != nil {
				b.log.Warn("dropping malformed event", zap.Error(err), zap.String("topic", msg.Channel))
				continue
			}
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Close terminates all active subscriptions.
func (b *RedisBus) Close() error {
	for _, ps := range b.subs {
		_ = ps.Close()
	}
	return nil
}
