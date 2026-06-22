package matchmaking

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
	"github.com/alpnuhoglu/gamemesh/pkg/metrics"
)

// capturingPublisher records published events in place of a real bus.
type capturingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *capturingPublisher) Publish(_ context.Context, _ string, e events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}

func (p *capturingPublisher) Close() error { return nil }

func newTestService(t *testing.T) (*Service, *capturingPublisher) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pub := &capturingPublisher{}
	svc := NewService(
		NewQueue(rdb),
		NewRoomStore(rdb, time.Hour),
		pub,
		metrics.New("matchmaking-test"),
		zap.NewNop(),
		Config{RankWindow: 100, BatchSize: 1000, MaxQueueAge: 5 * time.Minute},
	)
	return svc, pub
}

func TestMatchOnceCreatesRoomAndPublishes(t *testing.T) {
	svc, pub := newTestService(t)
	ctx := context.Background()

	require.NoError(t, svc.JoinQueue(ctx, "alice", 1000))
	require.NoError(t, svc.JoinQueue(ctx, "bob", 1050))

	matches, err := svc.MatchOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, matches)

	// Both players removed from the queue.
	_, size, err := svc.QueueStatus(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, int64(0), size)

	// MatchFound event published with a fetchable room.
	require.Len(t, pub.events, 1)
	assert.Equal(t, events.TypeMatchFound, pub.events[0].Type)

	var payload events.MatchFoundPayload
	require.NoError(t, json.Unmarshal(pub.events[0].Payload, &payload))
	assert.ElementsMatch(t, []string{"alice", "bob"}, payload.Players)

	room, err := svc.GetRoom(ctx, payload.RoomID)
	require.NoError(t, err)
	assert.Equal(t, "waiting", room.Status)
	assert.ElementsMatch(t, []string{"alice", "bob"}, room.Players)
}

func TestMatchOnceBatchesMultipleRooms(t *testing.T) {
	// Exercises the batched createRooms path: several pairs matched in one tick
	// must all be saved, dequeued and published.
	svc, pub := newTestService(t)
	ctx := context.Background()

	// Six players, tightly ranked → three pairs in one tick.
	for i, id := range []string{"p1", "p2", "p3", "p4", "p5", "p6"} {
		require.NoError(t, svc.JoinQueue(ctx, id, 1000+i))
	}

	matches, err := svc.MatchOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, matches)

	// All six players dequeued.
	_, size, err := svc.QueueStatus(ctx, "p1")
	require.NoError(t, err)
	assert.Equal(t, int64(0), size)

	// Exactly three MatchFound events, each with a fetchable room.
	require.Len(t, pub.events, 3)
	seen := map[string]bool{}
	for _, e := range pub.events {
		assert.Equal(t, events.TypeMatchFound, e.Type)
		var payload events.MatchFoundPayload
		require.NoError(t, json.Unmarshal(e.Payload, &payload))
		room, err := svc.GetRoom(ctx, payload.RoomID)
		require.NoError(t, err)
		assert.Equal(t, "waiting", room.Status)
		for _, p := range payload.Players {
			assert.Falsef(t, seen[p], "player %s matched twice", p)
			seen[p] = true
		}
	}
	assert.Len(t, seen, 6)
}

func TestMatchOnceRespectsRankWindow(t *testing.T) {
	svc, pub := newTestService(t)
	ctx := context.Background()

	require.NoError(t, svc.JoinQueue(ctx, "casual", 100))
	require.NoError(t, svc.JoinQueue(ctx, "pro", 5000))

	matches, err := svc.MatchOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, matches)
	assert.Empty(t, pub.events)

	queued, size, err := svc.QueueStatus(ctx, "casual")
	require.NoError(t, err)
	assert.True(t, queued)
	assert.Equal(t, int64(2), size)
}

func TestGetRoomNotFound(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.GetRoom(context.Background(), "missing-room")
	assert.ErrorIs(t, err, ErrRoomNotFound)
}
