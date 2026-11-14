package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alpnuhoglu/gamemesh/internal/matchmaking"
	"github.com/alpnuhoglu/gamemesh/internal/wsgateway"
)

// TestRoomParticipantsReadsMatchmakingRooms: the join check sees the rooms
// matchmaking saved, maps a missing room to wsgateway.ErrRoomNotFound and
// passes a Redis failure on as an error, which the hub treats as a rejection.
func TestRoomParticipantsReadsMatchmakingRooms(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := matchmaking.NewRoomStore(rdb, time.Hour)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, &matchmaking.Room{ID: "r1", Players: []string{"alice", "bob"}}))
	p := roomParticipants{store: store}

	ok, err := p.IsParticipant(ctx, "r1", "alice")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = p.IsParticipant(ctx, "r1", "mallory")
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = p.IsParticipant(ctx, "missing", "alice")
	assert.ErrorIs(t, err, wsgateway.ErrRoomNotFound)

	mr.Close()
	_, err = p.IsParticipant(ctx, "r1", "alice")
	require.Error(t, err)
	assert.NotErrorIs(t, err, wsgateway.ErrRoomNotFound)
}
