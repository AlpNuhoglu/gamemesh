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
)

// Topics (channels) events are published on.
const (
	TopicMatchmaking = "events.matchmaking"
	TopicLeaderboard = "events.leaderboard"
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

// Publisher sends events to a topic. Implementations must be safe for
// concurrent use.
type Publisher interface {
	Publish(ctx context.Context, topic string, e Event) error
	Close() error
}

// Subscriber delivers events from one or more topics on a channel.
type Subscriber interface {
	Subscribe(ctx context.Context, topics ...string) (<-chan Event, error)
	Close() error
}
