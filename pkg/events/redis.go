package events

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
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

// Publish marshals and publishes the event to a topic.
func (b *RedisBus) Publish(ctx context.Context, topic string, e Event) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return b.client.Publish(ctx, topic, raw).Err()
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
