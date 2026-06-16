package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNewEvent(t *testing.T) {
	e, err := New(TypeMatchFound, MatchFoundPayload{RoomID: "r1", Players: []string{"a", "b"}})
	require.NoError(t, err)
	assert.NotEmpty(t, e.ID)
	assert.Equal(t, TypeMatchFound, e.Type)
	assert.WithinDuration(t, time.Now(), e.Timestamp, time.Minute)

	var p MatchFoundPayload
	require.NoError(t, json.Unmarshal(e.Payload, &p))
	assert.Equal(t, "r1", p.RoomID)
}

func TestNewEventUnmarshalablePayload(t *testing.T) {
	_, err := New(TypeMatchFound, make(chan int)) // channels can't marshal
	assert.Error(t, err)
}

func TestRedisBusRoundTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	bus := NewRedisBus(rdb, zap.NewNop(), nil)
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := bus.Subscribe(ctx, TopicLeaderboard)
	require.NoError(t, err)

	sent, err := New(TypeLeaderboardUpdated, LeaderboardUpdatedPayload{PlayerID: "p1", Score: 42, Rank: 1})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, TopicLeaderboard, sent))

	select {
	case got := <-ch:
		assert.Equal(t, sent.ID, got.ID)
		assert.Equal(t, TypeLeaderboardUpdated, got.Type)
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}

func TestRedisBusDropsMalformedMessages(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	bus := NewRedisBus(rdb, zap.NewNop(), nil)
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := bus.Subscribe(ctx, TopicMatchmaking)
	require.NoError(t, err)

	// Garbage on the wire must be skipped, not crash or block.
	require.NoError(t, rdb.Publish(ctx, TopicMatchmaking, "not-json").Err())
	valid, err := New(TypeMatchFound, MatchFoundPayload{RoomID: "r1"})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, TopicMatchmaking, valid))

	select {
	case got := <-ch:
		assert.Equal(t, valid.ID, got.ID, "the malformed message is dropped, the valid one delivered")
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}
