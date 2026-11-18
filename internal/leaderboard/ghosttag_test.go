package leaderboard

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGhostStore(t *testing.T) (*GhostTagStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewGhostTagStore(rdb), mr
}

var day1 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func match(room string, at time.Time) GhostTagMatch {
	return GhostTagMatch{RoomID: room, Players: []string{"alice", "bob"}, EndedAt: at}
}

// alicePlus is a scoring rule in which alice wins: 40 + 5 per streak step
// (her streak including this win), bob gets 5 and loses his streak. It
// records how often it was called and with what.
type alicePlus struct {
	calls atomic.Int32
	mu    sync.Mutex
	prior map[string]int
}

func (r *alicePlus) score(prior map[string]int) map[string]GhostTagAward {
	r.calls.Add(1)
	r.mu.Lock()
	r.prior = prior
	r.mu.Unlock()
	streak := prior["alice"] + 1
	return map[string]GhostTagAward{
		"alice": {Delta: 40 + 5*streak, WinStreak: streak, Win: true, CaptureMs: 28400},
		"bob":   {Delta: 5, WinStreak: 0},
	}
}

func zscore(t *testing.T, mr *miniredis.Miniredis, key, member string) float64 {
	t.Helper()
	v, err := mr.ZScore(key, member)
	require.NoError(t, err, "%s has no %s", key, member)
	return v
}

// TestGhostTagSettleWrites: the first settlement of a match writes both
// boards, the stats and the records that keep it from being written again.
func TestGhostTagSettleWrites(t *testing.T) {
	s, mr := newGhostStore(t)
	rule := &alicePlus{}

	st, err := s.Settle(context.Background(), match("r1", day1), rule.score)
	require.NoError(t, err)
	assert.True(t, st.Ranked)
	assert.Equal(t, 45, st.Awards["alice"].Delta)

	assert.Equal(t, 45.0, zscore(t, mr, GhostTagAllTimeKey, "alice"))
	assert.Equal(t, 5.0, zscore(t, mr, GhostTagAllTimeKey, "bob"))
	assert.Equal(t, 45.0, zscore(t, mr, "lb:ghosttag:daily:2026-11-17", "alice"))
	assert.Equal(t, "1", mr.HGet("player:stats:alice", "win_streak"))
	assert.Equal(t, "1", mr.HGet("player:stats:alice", "wins"))
	assert.Equal(t, "28400", mr.HGet("player:stats:alice", "fastest_capture_ms"))
	assert.Equal(t, "0", mr.HGet("player:stats:bob", "win_streak"))
	assert.Equal(t, "", mr.HGet("player:stats:bob", "wins"), "a loss is not a win")
	assert.Positive(t, mr.TTL("lb:ghosttag:daily:2026-11-17"), "a day's board expires")
	assert.True(t, mr.Exists("ghosttag:settled:r1"))
}

// TestGhostTagSettleReadsTheStreak: the rule gets the streaks as they are in
// Redis, so the bonus builds on the stored streak.
func TestGhostTagSettleReadsTheStreak(t *testing.T) {
	s, mr := newGhostStore(t)
	mr.HSet("player:stats:alice", "win_streak", "2")
	rule := &alicePlus{}

	st, err := s.Settle(context.Background(), match("r1", day1), rule.score)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"alice": 2, "bob": 0}, rule.prior)
	assert.Equal(t, 55, st.Awards["alice"].Delta, "40 + 5 x 3")
	assert.Equal(t, "3", mr.HGet("player:stats:alice", "win_streak"))
}

// TestGhostTagSettleIsIdempotent: a retry of the same room returns the first
// settlement and writes nothing; the rule is not even asked.
func TestGhostTagSettleIsIdempotent(t *testing.T) {
	s, mr := newGhostStore(t)
	rule := &alicePlus{}
	ctx := context.Background()

	first, err := s.Settle(ctx, match("r1", day1), rule.score)
	require.NoError(t, err)
	again, err := s.Settle(ctx, match("r1", day1), rule.score)
	require.NoError(t, err)

	assert.Equal(t, first, again)
	assert.Equal(t, int32(1), rule.calls.Load())
	assert.Equal(t, 45.0, zscore(t, mr, GhostTagAllTimeKey, "alice"), "not doubled")
	assert.Equal(t, "1", mr.HGet("player:stats:alice", "wins"))
}

// TestGhostTagSamePairSameDay: the same two players' second match on a UTC
// day produces no points, whatever order they are listed in; on the next
// day it does.
func TestGhostTagSamePairSameDay(t *testing.T) {
	s, mr := newGhostStore(t)
	rule := &alicePlus{}
	ctx := context.Background()

	_, err := s.Settle(ctx, match("r1", day1), rule.score)
	require.NoError(t, err)
	swapped := GhostTagMatch{RoomID: "r2", Players: []string{"bob", "alice"}, EndedAt: day1.Add(6 * time.Hour)}
	st, err := s.Settle(ctx, swapped, rule.score)
	require.NoError(t, err)
	assert.False(t, st.Ranked)
	assert.Empty(t, st.Awards)
	assert.Equal(t, int32(1), rule.calls.Load(), "an unranked match is not scored")
	assert.Equal(t, 45.0, zscore(t, mr, GhostTagAllTimeKey, "alice"), "nothing written")

	// A retry of that room must keep its answer even once the pair key
	// is gone (it lives 48 h): the room's own record is what answers.
	mr.Del("ghosttag:pair:2026-11-17:alice:bob")
	again, err := s.Settle(ctx, swapped, rule.score)
	require.NoError(t, err)
	assert.False(t, again.Ranked, "the unranked answer is remembered for the room")
	assert.Equal(t, int32(1), rule.calls.Load())

	st, err = s.Settle(ctx, match("r3", day1.Add(24*time.Hour)), rule.score)
	require.NoError(t, err)
	assert.True(t, st.Ranked, "the next UTC day counts again")
	assert.Equal(t, 45.0+50.0, zscore(t, mr, GhostTagAllTimeKey, "alice"))
	assert.Equal(t, 50.0, zscore(t, mr, "lb:ghosttag:daily:2026-11-18", "alice"))
}

// TestGhostTagFastestCaptureIsKept: a slower capture does not replace the
// fastest one, a faster one does.
func TestGhostTagFastestCaptureIsKept(t *testing.T) {
	s, mr := newGhostStore(t)
	ctx := context.Background()
	withCapture := func(ms int) func(map[string]int) map[string]GhostTagAward {
		return func(map[string]int) map[string]GhostTagAward {
			return map[string]GhostTagAward{"alice": {Delta: 40, WinStreak: 1, Win: true, CaptureMs: ms}, "bob": {Delta: 5}}
		}
	}
	day := day1
	for i, ms := range []int{20000, 12000, 30000, 0} {
		day = day.Add(24 * time.Hour) // a new day, so the pair rule does not apply
		_, err := s.Settle(ctx, match("r"+string(rune('a'+i)), day), withCapture(ms))
		require.NoError(t, err)
	}
	assert.Equal(t, "12000", mr.HGet("player:stats:alice", "fastest_capture_ms"))
}

// TestGhostTagConcurrentSettleOfOneRoom: two settlements of the same room at
// once (a retry racing the first attempt) write it once.
func TestGhostTagConcurrentSettleOfOneRoom(t *testing.T) {
	s, mr := newGhostStore(t)
	rule := &alicePlus{}
	var wg sync.WaitGroup
	results := make([]GhostTagSettlement, 8)
	for i := range results {
		wg.Go(func() {
			st, err := s.Settle(context.Background(), match("r1", day1), rule.score)
			assert.NoError(t, err)
			results[i] = st
		})
	}
	wg.Wait()
	assert.Equal(t, 45.0, zscore(t, mr, GhostTagAllTimeKey, "alice"), "written once")
	assert.Equal(t, "1", mr.HGet("player:stats:alice", "wins"))
	for _, st := range results {
		assert.Equal(t, results[0], st, "every caller gets the same settlement")
	}
}

// TestGhostTagSettleFailsWithoutRedis: nothing can be written, and the
// caller is told.
func TestGhostTagSettleFailsWithoutRedis(t *testing.T) {
	s, mr := newGhostStore(t)
	mr.Close()
	_, err := s.Settle(context.Background(), match("r1", day1), (&alicePlus{}).score)
	require.Error(t, err)
}

// TestGhostTagRank: the all-time board's entry for a player.
func TestGhostTagRank(t *testing.T) {
	s, _ := newGhostStore(t)
	ctx := context.Background()
	_, err := s.Settle(ctx, match("r1", day1), (&alicePlus{}).score)
	require.NoError(t, err)

	e, err := s.GhostTagRank(ctx, "bob")
	require.NoError(t, err)
	assert.Equal(t, Entry{PlayerID: "bob", Score: 5, Rank: 2}, e)
	_, err = s.GhostTagRank(ctx, "carol")
	assert.ErrorIs(t, err, ErrPlayerNotRanked)
}
