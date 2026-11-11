package wsgateway

// Room relay: ephemeral game traffic (movement, position, input) from one
// client to the other members of a room.
//
//	client → server: {"action":"relay","room":"<id>","data":<any JSON value>}
//	server → others: {"type":"Relay","room":"<id>","from":"<sender>","data":<data>}
//
// The path is client → hub → clients, in memory and at-most-once. It never
// touches the event bus, Redis Pub/Sub or the outbox: those carry durable
// events (MatchFound, LeaderboardUpdated, ...) and must not see per-frame
// game traffic. This file therefore must not import pkg/events or
// internal/outbox. A relay reaches only the room members connected to this
// replica.
//
// A rejected message (invalid, too large, over the rate limit, sender not in
// the room) is dropped and counted; the connection stays open, no reply is
// sent and nothing is logged, since a client sends these many times a second.

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

const (
	// maxRelayData bounds the data value of one relay message. The
	// connection's read limit (maxMsgSize) stays the outer bound.
	maxRelayData = 1024

	// Default per-connection token bucket; see Hub.SetRelayLimit.
	defaultRelayRate  = 60
	defaultRelayBurst = 120
)

// relayMetrics holds the labelled instruments resolved once, so the hot path
// does no label lookup.
type relayMetrics struct {
	process, queue, write, total prometheus.Observer

	dropped prometheus.Counter

	notMember, tooLarge, rateLimited, invalid prometheus.Counter
}

func newRelayMetrics(m *metrics.Metrics) relayMetrics {
	stage := m.WSRelayDuration.WithLabelValues
	reason := m.WSRelayRejectedTotal.WithLabelValues
	return relayMetrics{
		process:     stage("process"),
		queue:       stage("queue"),
		write:       stage("write"),
		total:       stage("total"),
		dropped:     m.WSRelayDroppedTotal,
		notMember:   reason("not_member"),
		tooLarge:    reason("too_large"),
		rateLimited: reason("rate_limited"),
		invalid:     reason("invalid"),
	}
}

// relayState is a connection's relay state. Only its readPump touches it.
type relayState struct {
	limiter *rate.Limiter
	// prefix is the frame up to the data value for room, built once per room
	// rather than per message.
	room   string
	prefix []byte
}

// SetRelayLimit sets the per-connection relay token bucket: perSecond
// messages per second, up to burst at once. It applies to connections opened
// afterwards, so call it before serving.
func (h *Hub) SetRelayLimit(perSecond float64, burst int) {
	h.relayRate = rate.Limit(perSecond)
	h.relayBurst = burst
}

// handleRelay processes one inbound relay message that ReadMessage returned
// at readAt.
func (c *Client) handleRelay(msg *inboundMessage, readAt time.Time) {
	rm := &c.hub.relayM
	switch {
	case msg.Room == "" || len(msg.Data) == 0 || string(msg.Data) == "null":
		rm.invalid.Inc()
		return
	case len(msg.Data) > maxRelayData:
		rm.tooLarge.Inc()
		return
	case !c.relay.limiter.AllowN(readAt, 1):
		rm.rateLimited.Inc()
		return
	}
	if !c.hub.relayToRoom(c, msg.Room, c.relayFrame(msg.Room, msg.Data), readAt) {
		rm.notMember.Inc()
		return
	}
	rm.process.Observe(time.Since(readAt).Seconds())
}

// relayFrame builds the outbound frame. It is written by hand rather than
// with json.Marshal so that data goes out byte-for-byte (Marshal compacts and
// re-escapes a RawMessage). type and from are fixed by the server: from is
// the player the connection authenticated as, never anything the client sent.
// data is valid JSON, since the whole inbound message was decoded.
func (c *Client) relayFrame(room string, data []byte) []byte {
	if c.relay.prefix == nil || room != c.relay.room {
		r, _ := json.Marshal(room)
		from, _ := json.Marshal(c.playerID)
		p := append([]byte(`{"type":"Relay","room":`), r...)
		p = append(p, `,"from":`...)
		p = append(p, from...)
		c.relay.room, c.relay.prefix = room, append(p, `,"data":`...)
	}
	frame := make([]byte, 0, len(c.relay.prefix)+len(data)+1)
	frame = append(frame, c.relay.prefix...)
	frame = append(frame, data...)
	return append(frame, '}')
}

// relayToRoom queues frame, shared by all recipients, for every member of
// room except from, and reports whether from is a member. The membership
// check and the fan-out happen under one read lock, so a concurrent leave or
// disconnect cannot slip between them. A full send buffer sheds the frame,
// counted rather than logged.
func (h *Hub) relayToRoom(from *Client, room string, frame []byte, readAt time.Time) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	members := h.rooms[room]
	if _, ok := members[from]; !ok {
		return false
	}
	for c := range members {
		if c == from {
			continue
		}
		select {
		case c.send <- outbound{msg: frame, readAt: readAt, queuedAt: time.Now()}:
		default:
			h.relayM.dropped.Inc()
		}
	}
	return true
}

// writeRelay writes one relay frame and records its queue, write and total
// times. queue runs until the writer starts this write.
func (c *Client) writeRelay(out outbound) error {
	start := time.Now()
	if err := c.conn.WriteMessage(websocket.TextMessage, out.msg); err != nil {
		return err
	}
	end := time.Now()
	rm := &c.hub.relayM
	rm.queue.Observe(start.Sub(out.queuedAt).Seconds())
	rm.write.Observe(end.Sub(start).Seconds())
	rm.total.Observe(end.Sub(out.readAt).Seconds())
	return nil
}
