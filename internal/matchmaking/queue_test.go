package matchmaking

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestQueue(t *testing.T) (*Queue, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewQueue(rdb), mr
}

func TestQueueEnqueueAndSnapshot(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, "high", 2000))
	require.NoError(t, q.Enqueue(ctx, "low", 500))
	require.NoError(t, q.Enqueue(ctx, "mid", 1000))

	size, err := q.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), size)

	// Snapshot must come back sorted by rank ascending.
	tickets, err := q.Snapshot(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, []Ticket{{"low", 500}, {"mid", 1000}, {"high", 2000}}, tickets)
}

func TestQueueEnqueueIsIdempotent(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, "a", 1000))
	require.NoError(t, q.Enqueue(ctx, "a", 1100)) // re-join refreshes rank

	size, err := q.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), size)

	tickets, err := q.Snapshot(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1100, tickets[0].Rank)
}

func TestQueueRemove(t *testing.T) {
	q, _ := newTestQueue(t)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, "a", 1000))
	require.NoError(t, q.Remove(ctx, "a"))
	assert.ErrorIs(t, q.Remove(ctx, "a"), ErrNotQueued)

	queued, err := q.Contains(ctx, "a")
	require.NoError(t, err)
	assert.False(t, queued)
}

func TestQueueEvictStale(t *testing.T) {
	q, mr := newTestQueue(t)
	ctx := context.Background()

	require.NoError(t, q.Enqueue(ctx, "old", 1000))
	// Backdate the join timestamp beyond maxAge.
	mr.HSet(joinedKey, "old", "1000000000") // year 2001
	require.NoError(t, q.Enqueue(ctx, "fresh", 1000))

	evicted, err := q.EvictStale(ctx, 5*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, []string{"old"}, evicted)

	size, err := q.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), size)
}
