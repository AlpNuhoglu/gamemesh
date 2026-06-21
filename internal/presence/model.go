// Package presence tracks where every player is — OFFLINE, ONLINE, IN_QUEUE,
// IN_MATCH or AWAY — as a distributed, self-healing view backed by Redis.
//
// Presence is deliberately NOT a boolean. Redis is the source of truth: each
// player has a presence:{playerID} key carrying state, last_seen and a
// connection_count, with a TTL that heartbeats refresh. If heartbeats stop (a
// WS crash, a pod restart, a network partition) the key expires and the player
// is OFFLINE — no explicit disconnect is required and the system self-heals.
// Any WS replica can update any player, so no sticky sessions or leader
// election are needed.
package presence

import (
	"encoding/json"
	"time"
)

// State is a player's presence state. Values are stable strings shared on the
// wire (Redis JSON, presence events, HTTP) so consumers can compare them
// directly.
type State string

const (
	StateOffline State = "OFFLINE"
	StateOnline  State = "ONLINE"
	StateInQueue State = "IN_QUEUE"
	StateInMatch State = "IN_MATCH"
	StateAway    State = "AWAY"
)

// Valid reports whether s is a recognised state.
func (s State) Valid() bool {
	switch s {
	case StateOffline, StateOnline, StateInQueue, StateInMatch, StateAway:
		return true
	default:
		return false
	}
}

// CanTransition reports whether moving from `s` to `to` is allowed.
//
// The rule is deliberately permissive so future reconnect, rematch and party
// flows never require changing the state machine. Only two transitions are
// rejected — both require an active connection that an offline player does not
// have:
//
//	OFFLINE -> IN_QUEUE
//	OFFLINE -> IN_MATCH
//
// Everything else (ONLINE->IN_QUEUE, IN_MATCH->ONLINE, IN_QUEUE->IN_MATCH,
// AWAY->IN_QUEUE, …) is allowed. OFFLINE->ONLINE happens implicitly on connect.
func CanTransition(from, to State) bool {
	if !to.Valid() {
		return false
	}
	if from == StateOffline && (to == StateInQueue || to == StateInMatch) {
		return false
	}
	return true
}

// Record is the value stored at presence:{playerID}. It is marshalled to JSON
// for Redis; last_seen / updated_at are Unix seconds for compact, stable wire
// representation.
type Record struct {
	State           State `json:"state"`
	LastSeen        int64 `json:"last_seen"`
	ConnectionCount int   `json:"connection_count"`
	UpdatedAt       int64 `json:"updated_at"`
}

// MarshalBinary lets the Record be written directly with redis SET. go-redis
// calls this for any encoding.BinaryMarshaler argument.
func (r Record) MarshalBinary() ([]byte, error) { return json.Marshal(r) }

// offlineRecord is the zero-presence value returned for any player with no live
// key — a missing key IS OFFLINE.
func offlineRecord() Record {
	return Record{State: StateOffline, ConnectionCount: 0}
}

// Friend is one entry in a bulk friend-presence lookup.
type Friend struct {
	PlayerID string `json:"player_id"`
	State    State  `json:"state"`
	LastSeen int64  `json:"last_seen"`
}

// now is indirected so tests can pin time. Production uses time.Now.
var now = func() time.Time { return time.Now() }
