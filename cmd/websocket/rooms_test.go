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

	access, err := p.Access(ctx, "r1", "alice")
	require.NoError(t, err)
	assert.True(t, access.Participant)
	assert.Nil(t, access.Game, "matchmaking's rooms are opaque-relay rooms")

	access, err = p.Access(ctx, "r1", "mallory")
	require.NoError(t, err)
	assert.False(t, access.Participant)

	_, err = p.Access(ctx, "missing", "alice")
	assert.ErrorIs(t, err, wsgateway.ErrRoomNotFound)

	// A room whose record names a game is a validated room, described to
	// the hub as the record has it.
	created := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.Save(ctx, &matchmaking.Room{
		ID: "g1", Players: []string{"alice"}, Mode: matchmaking.ModeGhostTag, Bot: true, Unranked: true, CreatedAt: created,
	}))
	access, err = p.Access(ctx, "g1", "alice")
	require.NoError(t, err)
	assert.True(t, access.Participant)
	require.NotNil(t, access.Game)
	assert.Equal(t, wsgateway.GameSpec{
		Mode: matchmaking.ModeGhostTag, Players: []string{"alice"}, Bot: true, Unranked: true, CreatedAt: created,
	}, *access.Game)

	mr.Close()
	_, err = p.Access(ctx, "r1", "alice")
	require.Error(t, err)
	assert.NotErrorIs(t, err, wsgateway.ErrRoomNotFound)
}
