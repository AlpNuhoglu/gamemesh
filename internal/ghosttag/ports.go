// Package ghosttag is the server core of Ghost Tag, the two-player
// validated-room game specified in docs/ghosttag-design-v3.1.md: the room
// state machine, the movement validation pipeline, the ring buffers, the
// symmetric capture check, the bot and the scoring.
//
// The package knows nothing about WebSockets, Redis or the event bus. It
// reaches the outside through the three interfaces in this file, which the
// WebSocket service implements, and it must not import a transport, a store
// client, pkg/events or internal/wsgateway (boundary_test.go checks).
//
// Concurrency: the state of a room belongs to one goroutine, the one running
// Session.Run. Everything that changes it (a position packet, a presence
// change, the passing of time) reaches that goroutine through the session's
// channels and its one ticker, so the room itself needs and has no mutex.
// Session.Post, Session.Notify and Session.Done are the only methods other
// goroutines may call.
package ghosttag

import (
	"context"
	"time"
)

// Transport is how a room reaches its players. The WebSocket hub implements
// it, one value per room.
//
// A payload is one message of the design document's section 6, as a JSON
// object ({"t":"pos",...}); the implementation wraps it in the service's
// envelope. A payload may be shared by several recipients and must not be
// modified after the call. No method blocks on a slow player: delivery is at
// most once, and a message for a connection whose queue is full is dropped.
// The methods are called from the room's goroutine only.
type Transport interface {
	// Broadcast sends payload to every connection in the room.
	Broadcast(payload []byte)
	// BroadcastExcept sends payload to every connection in the room except
	// those of playerID. It carries a validated position; arrived is when
	// the packet that caused it was read from its sender's socket, which the
	// implementation uses to time the message through the server. A bot's
	// position has no such packet and passes the zero time.
	BroadcastExcept(playerID string, payload []byte, arrived time.Time)
	// SendTo sends payload to every connection of one player in the room.
	SendTo(playerID string, payload []byte)
	// Connected reports whether playerID has at least one connection in the
	// room at this moment. It is the truth about presence: Session.Notify
	// only says that the answer may have changed.
	Connected(playerID string) bool
	// Name is the player's display name as their connection in the room
	// says it, "" if it does not know one. To the game a name is an opaque
	// string it shows in snap and hands the score layer (Match.Names).
	Name(playerID string) string
}

// Match identifies a finished match to the score layer.
type Match struct {
	// RoomID is the idempotency key: a room is settled at most once.
	RoomID string
	// Players are the human players, in no particular order.
	Players []string
	// Names are the human players' display names by ID, as the room last
	// learned them; a player whose name it never learned is absent. The
	// store keeps them with the player's stats, for the board.
	Names map[string]string
	// EndedAt decides the UTC day of the daily board and of the same-pair
	// rule.
	EndedAt time.Time
}

// Standing is what the score layer knows about a player before a match is
// settled.
type Standing struct {
	// WinStreak is the player's current run of wins.
	WinStreak int
}

// Award is what a settled match does to one player.
type Award struct {
	// Delta is added to the player's score on the all-time and the daily
	// board. It is negative for a player who forfeited.
	Delta int
	// WinStreak is the player's streak after the match.
	WinStreak int
	// Win says the match counts towards the player's total wins.
	Win bool
	// CaptureMs is how long the player took to capture as hunter, or 0 for
	// no capture. The store keeps the fastest.
	CaptureMs int
}

// Settlement is the outcome of ScoreStore.Settle.
type Settlement struct {
	// Ranked is false when the match produced no points because the same
	// players had already settled a match on that UTC day. Nothing was
	// written then and Awards is empty.
	Ranked bool
	// Awards holds what was written, by player ID.
	Awards map[string]Award
}

// ScoreStore applies the result of a ranked match between humans. A bot
// match, a cancelled match and a room created unranked never reach it.
type ScoreStore interface {
	// Settle applies m exactly once and atomically: it reads the players'
	// standings, calls score with them, and writes the awards score returns.
	//
	// score is the game's scoring rule and must be a pure function: the
	// store may call it more than once (an optimistic transaction that is
	// retried), or not at all.
	//
	// A second Settle for the same RoomID writes nothing and returns the
	// first one's Settlement. If the same players already settled a match on
	// the UTC day of m.EndedAt, nothing is written and Ranked is false.
	//
	// On an error nothing was written, as far as the store can tell; the
	// room then reports the match as unranked rather than show the players
	// points that do not exist.
	Settle(ctx context.Context, m Match, score func(prior map[string]Standing) map[string]Award) (Settlement, error)
}

// Observer counts what happens in rooms, for telemetry. Implementations must
// not block. The events are a small fixed set (the Event constants), never a
// room or a player, so they are safe as a metric label.
type Observer interface {
	Count(event string)
}

// Events every implementation of the core reports. The core may define more
// next to the code that emits them.
const (
	// EventJoinTimeout: a match cancelled in WAITING because a player did
	// not join the room within Config.TJoin.
	EventJoinTimeout = "join_timeout"
	// EventBothDisconnected: a match voided because both players were gone
	// when the disconnect grace ran out.
	EventBothDisconnected = "both_disconnected"
	// EventClamp: the validation pipeline corrected a position.
	EventClamp = "clamp"
	// EventInboxDropped: a position packet dropped because the room's inbox
	// was full.
	EventInboxDropped = "inbox_dropped"
	// EventSettleFailed: the score store failed and the match was reported
	// unranked.
	EventSettleFailed = "settle_failed"
)
