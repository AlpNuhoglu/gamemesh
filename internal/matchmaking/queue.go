// Package matchmaking pairs queued players by rank into game rooms.
//
// Redis data structures used:
//   - matchmaking:queue (sorted set, score = rank): keeps the queue
//     permanently sorted by rank, so each match tick reads players already
//     ordered for windowed pairing in O(log n + m) — no sort step.
//   - matchmaking:queue:joined (hash, player → unix seconds): join timestamps
//     for evicting stale/disconnected players without scanning the ZSET.
//   - room:{id} (string, JSON, TTL): room cache; TTL gives free cleanup of
//     finished/abandoned rooms.
package matchmaking

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	queueKey  = "matchmaking:queue"
	joinedKey = "matchmaking:queue:joined"
)

// ErrNotQueued is returned when removing a player who is not in the queue.
var ErrNotQueued = errors.New("player not in queue")

// Ticket is one queued player.
type Ticket struct {
	PlayerID string `json:"player_id"`
	Rank     int    `json:"rank"`
}

// Queue is the Redis-backed matchmaking queue.
type Queue struct {
	rdb *redis.Client
}

// NewQueue wraps an existing Redis client.
func NewQueue(rdb *redis.Client) *Queue {
	return &Queue{rdb: rdb}
}

// Enqueue adds (or re-ranks) a player. Idempotent: re-joining just refreshes
// rank and join time.
//
// Cluster-ready: uses the non-transactional Pipeline() (not TxPipeline). ZADD
// (queueKey) and HSET (joinedKey) hash to different slots under Redis Cluster,
// where a TxPipeline would fail cross-slot; Pipeline() lets a ClusterClient
// split them. The two writes are independent, so dropping atomicity is safe — a
// re-join overwrites both anyway.
func (q *Queue) Enqueue(ctx context.Context, playerID string, rank int) error {
	pipe := q.rdb.Pipeline()
	pipe.ZAdd(ctx, queueKey, redis.Z{Score: float64(rank), Member: playerID})
	pipe.HSet(ctx, joinedKey, playerID, time.Now().Unix())
	_, err := pipe.Exec(ctx)
	return err
}

// Remove deletes a player from the queue.
func (q *Queue) Remove(ctx context.Context, playerID string) error {
	removed, err := q.rdb.ZRem(ctx, queueKey, playerID).Result()
	if err != nil {
		return err
	}
	q.rdb.HDel(ctx, joinedKey, playerID)
	if removed == 0 {
		return ErrNotQueued
	}
	return nil
}

// RemoveBatch deletes multiple players from the queue (used after a match tick).
//
// Cluster-ready: it uses the non-transactional Pipeline() (not TxPipeline). The
// ZREM (queueKey) and HDEL (joinedKey) hash to different slots under Redis
// Cluster, so a TxPipeline would error as a cross-slot transaction; Pipeline()
// lets a ClusterClient split them safely. Atomicity is intentionally traded
// away — a player left half-removed is simply re-paired on the next tick
// (self-correcting), and an orphaned room is reclaimed by its TTL.
//
// Allocation-free argument passing: ZRem is variadic (...interface{}) but
// go-redis special-cases a single []string argument (appendArgs -> appendArg),
// boxing the strings itself — so passing playerIDs as one []string avoids
// building an intermediate []any and the per-element heap escape. HDel already
// takes ...string, so playerIDs flows through with no boxing at all.
func (q *Queue) RemoveBatch(ctx context.Context, playerIDs []string) error {
	if len(playerIDs) == 0 {
		return nil
	}
	pipe := q.rdb.Pipeline()
	pipe.ZRem(ctx, queueKey, playerIDs)
	pipe.HDel(ctx, joinedKey, playerIDs...)
	_, err := pipe.Exec(ctx)
	return err
}

// Contains reports whether a player is queued.
func (q *Queue) Contains(ctx context.Context, playerID string) (bool, error) {
	_, err := q.rdb.ZScore(ctx, queueKey, playerID).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Size returns the queue length. O(1) on a ZSET.
func (q *Queue) Size(ctx context.Context) (int64, error) {
	return q.rdb.ZCard(ctx, queueKey).Result()
}

// Snapshot returns up to limit tickets ordered by rank ascending.
func (q *Queue) Snapshot(ctx context.Context, limit int64) ([]Ticket, error) {
	zs, err := q.rdb.ZRangeWithScores(ctx, queueKey, 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	tickets := make([]Ticket, 0, len(zs))
	for _, z := range zs {
		tickets = append(tickets, Ticket{PlayerID: z.Member.(string), Rank: int(z.Score)})
	}
	return tickets, nil
}

// EvictStale removes players whose tickets are older than maxAge — this
// covers disconnected clients that never sent an explicit leave. Returns the
// evicted player IDs.
func (q *Queue) EvictStale(ctx context.Context, maxAge time.Duration) ([]string, error) {
	joined, err := q.rdb.HGetAll(ctx, joinedKey).Result()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-maxAge).Unix()
	var stale []string
	for playerID, ts := range joined {
		joinedAt, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || joinedAt < cutoff {
			stale = append(stale, playerID)
		}
	}
	if len(stale) == 0 {
		return nil, nil
	}
	return stale, q.RemoveBatch(ctx, stale)
}
