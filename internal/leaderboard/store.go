// Package leaderboard implements global rankings on Redis sorted sets.
//
// Why a sorted set: ZADD and ZINCRBY are O(log n), ZREVRANK is O(log n) and
// ZREVRANGE is O(log n + m) — exactly the complexity bounds required, and
// they hold at millions of members. A SQL ORDER BY ... LIMIT would be O(n
// log n) or demand heavy index maintenance under write-intensive game
// traffic.
package leaderboard

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

// Key holds the single global leaderboard. Sharding strategy for multiple
// boards (per-season, per-mode) is simply more keys: "leaderboard:{season}".
const Key = "leaderboard:global"

// ErrPlayerNotRanked is returned when a player has no score yet.
var ErrPlayerNotRanked = errors.New("player not ranked")

// Entry is one leaderboard row. Rank is 1-based.
type Entry struct {
	PlayerID string  `json:"player_id"`
	Score    float64 `json:"score"`
	Rank     int64   `json:"rank"`
}

// Store wraps the Redis operations behind a small interface-free struct; the
// service layer is the seam used for mocking in tests, while store tests run
// against miniredis.
type Store struct {
	rdb *redis.Client
}

// NewStore wraps an existing Redis client.
func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

// SetScore overwrites the player's score. O(log n).
func (s *Store) SetScore(ctx context.Context, playerID string, score float64) error {
	return s.rdb.ZAdd(ctx, Key, redis.Z{Score: score, Member: playerID}).Err()
}

// IncrScore adds delta to the player's score and returns the new value.
// O(log n).
func (s *Store) IncrScore(ctx context.Context, playerID string, delta float64) (float64, error) {
	return s.rdb.ZIncrBy(ctx, Key, delta, playerID).Result()
}

// Top returns the n highest-scored players. O(log n + m).
func (s *Store) Top(ctx context.Context, n int64) ([]Entry, error) {
	return s.rangeDesc(ctx, 0, n-1)
}

// Page returns a slice of the leaderboard plus the total member count.
func (s *Store) Page(ctx context.Context, offset, limit int64) ([]Entry, int64, error) {
	entries, err := s.rangeDesc(ctx, offset, offset+limit-1)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.rdb.ZCard(ctx, Key).Result()
	if err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// Rank returns a single player's entry. O(log n).
func (s *Store) Rank(ctx context.Context, playerID string) (Entry, error) {
	rank, err := s.rdb.ZRevRank(ctx, Key, playerID).Result()
	if errors.Is(err, redis.Nil) {
		return Entry{}, ErrPlayerNotRanked
	}
	if err != nil {
		return Entry{}, err
	}
	score, err := s.rdb.ZScore(ctx, Key, playerID).Result()
	if err != nil {
		return Entry{}, err
	}
	return Entry{PlayerID: playerID, Score: score, Rank: rank + 1}, nil
}

func (s *Store) rangeDesc(ctx context.Context, start, stop int64) ([]Entry, error) {
	zs, err := s.rdb.ZRevRangeWithScores(ctx, Key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(zs))
	for i, z := range zs {
		entries = append(entries, Entry{
			PlayerID: z.Member.(string),
			Score:    z.Score,
			Rank:     start + int64(i) + 1,
		})
	}
	return entries, nil
}
