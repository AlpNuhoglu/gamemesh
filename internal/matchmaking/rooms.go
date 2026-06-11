package matchmaking

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrRoomNotFound is returned for missing or expired rooms.
var ErrRoomNotFound = errors.New("room not found")

// Room is a created match. Stored as JSON in Redis with a TTL so abandoned
// rooms clean themselves up.
type Room struct {
	ID        string         `json:"id"`
	Players   []string       `json:"players"`
	Ranks     map[string]int `json:"ranks"`
	Status    string         `json:"status"`
	CreatedAt time.Time      `json:"created_at"`
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
