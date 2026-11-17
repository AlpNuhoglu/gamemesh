package matchmaking

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrRoomNotFound is returned for missing or expired rooms.
var ErrRoomNotFound = errors.New("room not found")

// ModeGhostTag is the Mode of a room that is a Ghost Tag match.
const ModeGhostTag = "ghosttag"

// Room is a created match. Stored as JSON in Redis with a TTL so abandoned
// rooms clean themselves up.
//
// Mode, Bot and Unranked describe a validated room: one whose traffic the
// WebSocket service reads and whose game it runs. A room without a Mode is
// an opaque-relay room, as every room was before these fields existed; they
// are omitted when empty, so such a room's record is unchanged. The record
// is the only thing that decides what kind of room it is: a client cannot.
type Room struct {
	ID        string         `json:"id"`
	Players   []string       `json:"players"`
	Ranks     map[string]int `json:"ranks"`
	Status    string         `json:"status"`
	CreatedAt time.Time      `json:"created_at"`
	// Mode names the game of a validated room (ModeGhostTag), empty for an
	// opaque-relay room.
	Mode string `json:"mode,omitempty"`
	// Bot says the room has one human player and the server's bot plays
	// the other part.
	Bot bool `json:"bot,omitempty"`
	// Unranked says the match must not produce points (bench rooms).
	Unranked bool `json:"unranked,omitempty"`
}

// HasPlayer reports whether playerID is one of the room's players.
func (r *Room) HasPlayer(playerID string) bool {
	return slices.Contains(r.Players, playerID)
}

// RoomStore caches rooms in Redis.
type RoomStore struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRoomStore wraps an existing Redis client.
func NewRoomStore(rdb *redis.Client, ttl time.Duration) *RoomStore {
	return &RoomStore{rdb: rdb, ttl: ttl}
}

func roomKey(id string) string { return "room:" + id }

// Save writes the room with the configured TTL.
func (s *RoomStore) Save(ctx context.Context, room *Room) error {
	raw, err := json.Marshal(room)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, roomKey(room.ID), raw, s.ttl).Err()
}

// SaveMany writes many rooms in a single pipelined round trip — the batch
// fast-path for a match tick, replacing one SET round trip per room.
//
// Cluster-ready: it uses the non-transactional Pipeline() (not TxPipeline), so a
// go-redis ClusterClient can split the per-room SETs across the slots their keys
// hash to. Every SET is single-key, so there is no cross-slot assumption and no
// hash tag is needed.
func (s *RoomStore) SaveMany(ctx context.Context, rooms []*Room) error {
	if len(rooms) == 0 {
		return nil
	}
	pipe := s.rdb.Pipeline()
	// Marshal up front so a JSON error fails before any network work is queued.
	for _, room := range rooms {
		raw, err := json.Marshal(room)
		if err != nil {
			return err
		}
		pipe.Set(ctx, roomKey(room.ID), raw, s.ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Get fetches a room by ID.
func (s *RoomStore) Get(ctx context.Context, id string) (*Room, error) {
	raw, err := s.rdb.Get(ctx, roomKey(id)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrRoomNotFound
	}
	if err != nil {
		return nil, err
	}
	var room Room
	if err := json.Unmarshal([]byte(raw), &room); err != nil {
		return nil, err
	}
	return &room, nil
}
