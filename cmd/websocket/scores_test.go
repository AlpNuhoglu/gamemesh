package main

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

	"github.com/alpnuhoglu/gamemesh/internal/ghosttag"
	"github.com/alpnuhoglu/gamemesh/internal/leaderboard"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

type publishedEvents struct {
	mu     sync.Mutex
	topics []string
	events []events.Event
}

func (p *publishedEvents) Publish(_ context.Context, topic string, e events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topics = append(p.topics, topic)
	p.events = append(p.events, e)
	return nil
}

func (p *publishedEvents) Close() error { return nil }

func (p *publishedEvents) payloads(t *testing.T) []events.LeaderboardUpdatedPayload {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []events.LeaderboardUpdatedPayload
	for i, e := range p.events {
		require.Equal(t, events.TypeLeaderboardUpdated, e.Type)
		require.Equal(t, events.TopicLeaderboard, p.topics[i])
		var pl events.LeaderboardUpdatedPayload
		require.NoError(t, json.Unmarshal(e.Payload, &pl))
		out = append(out, pl)
	}
	return out
}

func newGameScores(t *testing.T) (gameScores, *miniredis.Miniredis, *publishedEvents) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	pub := &publishedEvents{}
	return gameScores{
		store: leaderboard.NewGhostTagStore(rdb),
		pub:   pub,
		log:   zap.NewNop(),
		later: func(f func()) { f() }, // inline, so the test sees the events
	}, mr, pub
}

var settledAt = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// aWins is a scoring rule in which a wins with her streak plus one; it
// records the standings it was given.
func aWins(seen *map[string]ghosttag.Standing) func(map[string]ghosttag.Standing) map[string]ghosttag.Award {
	return func(prior map[string]ghosttag.Standing) map[string]ghosttag.Award {
		*seen = prior
		streak := prior["a"].WinStreak + 1
		return map[string]ghosttag.Award{
			"a": {Delta: 40 + 5*streak, WinStreak: streak, Win: true, CaptureMs: 12000},
			"b": {Delta: 5},
		}
	}
}

// TestGameScoresSettlesAndAnnounces: the game's rule sees the streaks in
// Redis, its awards are written and returned, and each player's new
// standing on the Ghost Tag board is announced, marked as that board.
func TestGameScoresSettlesAndAnnounces(t *testing.T) {
	g, mr, pub := newGameScores(t)
	mr.HSet("player:stats:a", "win_streak", "1")
	var seen map[string]ghosttag.Standing

	st, err := g.Settle(context.Background(), ghosttag.Match{RoomID: "r1", Players: []string{"a", "b"}, EndedAt: settledAt}, aWins(&seen))
	require.NoError(t, err)

	assert.Equal(t, map[string]ghosttag.Standing{"a": {WinStreak: 1}, "b": {}}, seen)
	assert.True(t, st.Ranked)
	assert.Equal(t, ghosttag.Award{Delta: 50, WinStreak: 2, Win: true, CaptureMs: 12000}, st.Awards["a"])
	assert.Equal(t, ghosttag.Award{Delta: 5}, st.Awards["b"])
	score, err := mr.ZScore(leaderboard.GhostTagAllTimeKey, "a")
	require.NoError(t, err)
	assert.Equal(t, 50.0, score)

	got := pub.payloads(t)
	require.Len(t, got, 2)
	assert.ElementsMatch(t, []events.LeaderboardUpdatedPayload{
		{PlayerID: "a", Score: 50, Rank: 1, Board: events.BoardGhostTag},
		{PlayerID: "b", Score: 5, Rank: 2, Board: events.BoardGhostTag},
	}, got)
}

// TestGameScoresUnrankedIsNotAnnounced: the same pair's second match of the
// day writes nothing, and nothing is announced for it.
func TestGameScoresUnrankedIsNotAnnounced(t *testing.T) {
	g, _, pub := newGameScores(t)
	var seen map[string]ghosttag.Standing
	ctx := context.Background()
	_, err := g.Settle(ctx, ghosttag.Match{RoomID: "r1", Players: []string{"a", "b"}, EndedAt: settledAt}, aWins(&seen))
	require.NoError(t, err)
	before := len(pub.payloads(t))

	st, err := g.Settle(ctx, ghosttag.Match{RoomID: "r2", Players: []string{"a", "b"}, EndedAt: settledAt.Add(time.Hour)}, aWins(&seen))
	require.NoError(t, err)
	assert.False(t, st.Ranked)
	assert.Empty(t, st.Awards)
	assert.Len(t, pub.payloads(t), before, "nothing announced for an unranked match")
}

// TestGameScoresFailureIsAnError: with Redis gone the room gets an error
// (and reports the match unranked); nothing is announced.
func TestGameScoresFailureIsAnError(t *testing.T) {
	g, mr, pub := newGameScores(t)
	mr.Close()
	var seen map[string]ghosttag.Standing
	_, err := g.Settle(context.Background(), ghosttag.Match{RoomID: "r1", Players: []string{"a", "b"}, EndedAt: settledAt}, aWins(&seen))
	require.Error(t, err)
	assert.Empty(t, pub.payloads(t))
}
