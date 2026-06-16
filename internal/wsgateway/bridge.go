package wsgateway

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/tracing"
)

// Bridge subscribes to backend events and fans them out to WebSocket clients.
// Every WS replica runs its own bridge: Redis Pub/Sub delivers each event to
// all replicas, so a player connected to any replica still receives it.
type Bridge struct {
	hub *Hub
	sub events.Subscriber
	log *zap.Logger
}

// NewBridge wires the bridge.
func NewBridge(hub *Hub, sub events.Subscriber, log *zap.Logger) *Bridge {
	return &Bridge{hub: hub, sub: sub, log: log}
}

// Run consumes events until ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	ch, err := b.sub.Subscribe(ctx, events.TopicMatchmaking, events.TopicLeaderboard)
	if err != nil {
		return err
	}
	b.log.Info("event bridge started")
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			b.dispatch(ctx, e)
		}
	}
}

func (b *Bridge) dispatch(ctx context.Context, e events.Event) {
	// Extract the trace context the publisher embedded in the event and start a
	// consumer span linked to the producing trace, so matchmaking-tick ->
	// publish -> ws-dispatch shows as one trace across Redis Pub/Sub.
	ctx, span := events.ReceiveSpan(ctx, e, "ws.dispatch")
	defer span.End()

	switch e.Type {
	case events.TypeMatchFound:
		var p events.MatchFoundPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			tracing.RecordError(span, err)
			b.log.Warn("malformed MatchFound payload", zap.Error(err))
			return
		}
		span.SetAttributes(attribute.Int("ws.recipients", len(p.Players)))
		msg := marshal(Message{Type: events.TypeMatchFound, Room: p.RoomID, Data: e.Payload})
		// Matched players may not be in any room yet — target them directly.
		for _, playerID := range p.Players {
			b.hub.SendToPlayer(playerID, msg)
		}
	case events.TypeLeaderboardUpdated:
		span.SetAttributes(attribute.String("ws.fanout", "broadcast"))
		b.hub.BroadcastAll(marshal(Message{Type: events.TypeLeaderboardUpdated, Data: e.Payload}))
	default:
		b.log.Debug("ignoring event", zap.String("type", e.Type))
	}
}
