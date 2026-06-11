package leaderboard

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewStore(rdb)
}

func TestSetScoreAndRank(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SetScore(ctx, "alice", 300))
	require.NoError(t, s.SetScore(ctx, "bob", 500))
	require.NoError(t, s.SetScore(ctx, "charlie", 100))

	entry, err := s.Rank(ctx, "bob")
	require.NoError(t, err)
	assert.Equal(t, Entry{PlayerID: "bob", Score: 500, Rank: 1}, entry)

	entry, err = s.Rank(ctx, "charlie")
	require.NoError(t, err)
	assert.Equal(t, int64(3), entry.Rank)
}

func TestIncrScore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	newScore, err := s.IncrScore(ctx, "alice", 100)
	require.NoError(t, err)
	assert.Equal(t, float64(100), newScore)

	newScore, err = s.IncrScore(ctx, "alice", 50)
	require.NoError(t, err)
	assert.Equal(t, float64(150), newScore)
}

func TestTopOrdering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for player, score := range map[string]float64{
		"d": 40, "a": 100, "c": 60, "b": 80, "e": 20,
	} {
		require.NoError(t, s.SetScore(ctx, player, score))
	}

	top, err := s.Top(ctx, 3)
	require.NoError(t, err)
	assert.Equal(t, []Entry{
		{PlayerID: "a", Score: 100, Rank: 1},
		{PlayerID: "b", Score: 80, Rank: 2},
		{PlayerID: "c", Score: 60, Rank: 3},
	}, top)
}

func TestPagePagination(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.SetScore(ctx, "a", 100))
	require.NoError(t, s.SetScore(ctx, "b", 80))
	require.NoError(t, s.SetScore(ctx, "c", 60))
	require.NoError(t, s.SetScore(ctx, "d", 40))

	entries, total, err := s.Page(ctx, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(4), total)
	assert.Equal(t, []Entry{
		{PlayerID: "b", Score: 80, Rank: 2},
		{PlayerID: "c", Score: 60, Rank: 3},
	}, entries)
}

func TestRankUnknownPlayer(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Rank(context.Background(), "ghost")
	assert.ErrorIs(t, err, ErrPlayerNotRanked)
}
