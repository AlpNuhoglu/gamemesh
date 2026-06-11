//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/leaderboard"
	"github.com/alpnuhoglu/gamemesh/internal/matchmaking"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

func startRedis(t *testing.T) *goredis.Client {
	t.Helper()
	ctx := context.Background()

	redisC, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisC.Terminate(context.Background()) })

	uri, err := redisC.ConnectionString(ctx)
	require.NoError(t, err)
	opts, err := goredis.ParseURL(uri)
	require.NoError(t, err)

	client := goredis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(ctx).Err())
	return client
}

func TestLeaderboardAgainstRealRedis(t *testing.T) {
	rdb := startRedis(t)
	store := leaderboard.NewStore(rdb)
	ctx := context.Background()

	for i, p := range []string{"p1", "p2", "p3", "p4", "p5"} {
		require.NoError(t, store.SetScore(ctx, p, float64((i+1)*100)))
	}

	top, err := store.Top(ctx, 2)
	require.NoError(t, err)
	require.Len(t, top, 2)
	assert.Equal(t, "p5", top[0].PlayerID)
	assert.Equal(t, int64(1), top[0].Rank)

	entry, err := store.Rank(ctx, "p1")
	require.NoError(t, err)
	assert.Equal(t, int64(5), entry.Rank)
}

func TestMatchmakingQueueAgainstRealRedis(t *testing.T) {
	rdb := startRedis(t)
	queue := matchmaking.NewQueue(rdb)
	ctx := context.Background()

	require.NoError(t, queue.Enqueue(ctx, "alice", 1000))
	require.NoError(t, queue.Enqueue(ctx, "bob", 1040))

	tickets, err := queue.Snapshot(ctx, 10)
	require.NoError(t, err)
	require.Len(t, tickets, 2)

	pairs := matchmaking.Pair(tickets, 100)
	require.Len(t, pairs, 1)

	require.NoError(t, queue.RemoveBatch(ctx, []string{"alice", "bob"}))
	size, err := queue.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), size)
}

func TestRedisBusPubSub(t *testing.T) {
	rdb := startRedis(t)
	bus := events.NewRedisBus(rdb, zap.NewNop())
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ch, err := bus.Subscribe(ctx, events.TopicMatchmaking)
	require.NoError(t, err)

	e, err := events.New(events.TypeMatchFound, events.MatchFoundPayload{
		RoomID:  "room-1",
		Players: []string{"alice", "bob"},
	})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, events.TopicMatchmaking, e))

	select {
	case got := <-ch:
		assert.Equal(t, events.TypeMatchFound, got.Type)
		var payload events.MatchFoundPayload
		require.NoError(t, json.Unmarshal(got.Payload, &payload))
		assert.Equal(t, "room-1", payload.RoomID)
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}
