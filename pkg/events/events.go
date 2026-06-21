// Package events defines the inter-service event contract behind small
// Publisher/Subscriber interfaces. Services depend only on these interfaces,
// so the Redis Pub/Sub transport used today can be swapped for NATS, Kafka or
// gRPC streaming by adding a new implementation — no service code changes.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event types broadcast through the system.
const (
	TypePlayerJoined       = "PlayerJoined"
	TypePlayerLeft         = "PlayerLeft"
	TypeMatchFound         = "MatchFound"
	TypeLeaderboardUpdated = "LeaderboardUpdated"
	// Player identity events. These flow through the transactional outbox: they
	// are written to outbox_events in the SAME Postgres transaction as the
	// players/player_stats rows, then relayed to NATS — so the event can never
	// be lost once the registration/update commits (no dual-write).
	TypePlayerRegistered = "PlayerRegistered"
	TypePlayerUpdated    = "PlayerUpdated"
	// Presence events. Emitted by the Presence Service on connection-driven
	// transitions. Published directly to NATS (not via the outbox): presence is
	// high-churn and self-correcting — a dropped event is re-derived on the next
	// heartbeat, so durability is unnecessary and the latency of the outbox poll
	// loop is undesirable.
	TypePresenceOnline       = "PresenceOnline"
	TypePresenceOffline      = "PresenceOffline"
	TypePresenceStateChanged = "PresenceStateChanged"
)

// Topics (channels) events are published on.
const (
	TopicMatchmaking = "events.matchmaking"
	TopicLeaderboard = "events.leaderboard"
	// TopicPlayer carries identity lifecycle events emitted via the outbox.
	TopicPlayer = "events.player"
	// TopicPresence carries player presence transitions (online/offline/state
	// changes). High-churn and short-lived — see the PRESENCE stream's MaxAge.
	TopicPresence = "events.presence"
)

// Event is the wire format for all inter-service messages.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
	// Carrier holds W3C trace-context headers (traceparent/tracestate) so a
	// trace propagates across the async Pub/Sub boundary. It is populated and
	// read purely via the OTel propagator, so it is transport-agnostic: a
	// future Kafka/NATS Publisher reuses the same field with the same
	// inject/extract calls — no change to the event contract. omitempty keeps
	// it backward compatible with consumers that ignore it.
	Carrier map[string]string `json:"carrier,omitempty"`
}

// New builds an event with a generated ID and the payload marshalled to JSON.
func New(eventType string, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:        uuid.NewString(),
		Type:      eventType,
		Timestamp: time.Now().UTC(),
		Payload:   raw,
	}, nil
}

// MatchFoundPayload is emitted when matchmaking pairs players into a room.
type MatchFoundPayload struct {
	RoomID  string   `json:"room_id"`
	Players []string `json:"players"`
}

// LeaderboardUpdatedPayload is emitted on every score change.
type LeaderboardUpdatedPayload struct {
	PlayerID string  `json:"player_id"`
	Score    float64 `json:"score"`
	Rank     int64   `json:"rank"`
}

// PlayerRegisteredPayload is emitted when a new player is created. Written to
// the outbox atomically with the players/player_stats rows.
type PlayerRegisteredPayload struct {
	PlayerID string `json:"player_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

// PlayerUpdatedPayload is emitted when a player edits their profile.
type PlayerUpdatedPayload struct {
	PlayerID string `json:"player_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

// PresenceChangedPayload is emitted on every presence transition. It carries
// both the new and previous state plus the live connection count so downstream
// consumers (friends, parties, notifications) have full context without an extra
// lookup. PreviousState is empty when the player had no prior presence record.
type PresenceChangedPayload struct {
	PlayerID        string `json:"player_id"`
	State           string `json:"state"`
	PreviousState   string `json:"previous_state,omitempty"`
	ConnectionCount int    `json:"connection_count"`
	LastSeen        int64  `json:"last_seen"` // Unix seconds
}

// Publisher sends events to a topic. Implementations must be safe for
// concurrent use.
type Publisher interface {
	Publish(ctx context.Context, topic string, e Event) error
	Close() error
}

// Subscriber delivers events from one or more topics on a channel.
//
// This is a fire-and-forget push model with no acknowledgement hook: it fits
// at-most-once transports (Redis Pub/Sub) where a missed message is acceptable.
// Transports that support acknowledgements (NATS JetStream) ALSO implement
// AckSubscriber below; callers needing at-least-once type-assert for it.
type Subscriber interface {
	Subscribe(ctx context.Context, topics ...string) (<-chan Event, error)
	Close() error
}

// Handler processes a single consumed event. Returning nil signals successful
// processing (the transport may ACK); a non-nil error signals failure (the
// transport may NAK and redeliver). Handlers must be safe for concurrent use:
// AckSubscriber implementations may invoke them from a bounded worker pool.
type Handler func(ctx context.Context, e Event) error

// AckSubscriber is the richer, opt-in consumer contract for durable transports.
// It is intentionally separate from Subscriber so the existing interface — and
// every service depending on it — stays unchanged (additive evolution, not a
// breaking change). A transport such as NATSBus implements BOTH: Subscribe for
// drop-in compatibility, SubscribeAck for genuine at-least-once delivery.
//
// SubscribeAck delivers each event to handler and ACKs only after handler
// returns nil, giving "ACK after successful processing" semantics with retry on
// error. It blocks until ctx is cancelled, draining in-flight work on shutdown.
type AckSubscriber interface {
	SubscribeAck(ctx context.Context, handler Handler, topics ...string) error
}
