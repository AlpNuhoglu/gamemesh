package wsgateway

// EXPERIMENT — branch exp/relay-jetstream, never merged, to be thrown away.
//
// It reroutes the relay through NATS JetStream to measure what the durable
// route costs for the same message, next to the direct path in relay.go:
//
//	readPump: checks, membership → Publish(events.relay.Relay), waits for the PubAck
//	JetStream: stores the message in the RELAY stream (file storage)
//	Bridge (this service's own consumer): pulls it, dispatch → deliverRelay
//	deliverRelay: queues the frame for the room's other members → writePump
//
// It uses the bus as the backend events do: events.New, NATSBus.Publish
// (synchronous, waits for the JetStream ack), and the Bridge's SubscribeAck
// consumer with its worker pool and explicit acks.
//
// The read time and the sending connection travel out of band, in
// relayPending keyed by event ID, so total stays on the monotonic clock. That
// only works because publisher and consumer are one process (one replica);
// a message this process did not publish is not delivered.

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// pendingRelay is what the delivery side needs from the sender, kept in
// process rather than sent over the bus.
type pendingRelay struct {
	from   *Client
	readAt time.Time
}

// SetRelayBus routes every relay through pub (EXPERIMENT). Call it before
// serving; with no bus set the relay takes the direct path.
func (h *Hub) SetRelayBus(pub events.Publisher) { h.relayBus = pub }

// isMember reports whether c is in room.
func (h *Hub) isMember(c *Client, room string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.rooms[room][c]
	return ok
}

// publishRelay puts one accepted relay frame on JetStream and returns when
// the JetStream ack arrives (or Publish fails).
func (c *Client) publishRelay(room string, frame []byte, readAt time.Time) error {
	e, err := events.New(events.TypeRelay, events.RelayPayload{Room: room, Frame: string(frame)})
	if err != nil {
		return err
	}
	// Stored before Publish: the consumer may receive the message before
	// Publish returns.
	c.hub.relayPending.Store(e.ID, pendingRelay{from: c, readAt: readAt})
	if err := c.hub.relayBus.Publish(context.Background(), events.TopicRelay, e); err != nil {
		c.hub.relayPending.Delete(e.ID)
		c.log.Warn("relay publish failed", zap.Error(err))
		return err
	}
	return nil
}

// deliverRelay queues a relay frame that came back from JetStream for every
// member of its room except the sending connection. Membership is the room's
// at delivery time. It always returns nil: a relay is never redelivered.
func (h *Hub) deliverRelay(e events.Event) error {
	var p events.RelayPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil
	}
	v, ok := h.relayPending.LoadAndDelete(e.ID)
	if !ok {
		return nil // not published by this process, or a redelivery
	}
	pr := v.(pendingRelay)
	frame := []byte(p.Frame)

	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[p.Room] {
		if c == pr.from {
			continue
		}
		select {
		case c.send <- outbound{msg: frame, readAt: pr.readAt, queuedAt: time.Now()}:
		default:
			h.relayM.dropped.Inc()
		}
	}
	return nil
}
