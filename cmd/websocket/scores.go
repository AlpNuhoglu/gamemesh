package main

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/ghosttag"
	"github.com/alpnuhoglu/gamemesh/internal/leaderboard"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// announceTimeout bounds the LeaderboardUpdated announcements of one
// settlement. They run after the room has its answer, so they never delay a
// match_end.
const announceTimeout = 5 * time.Second

// gameScores settles ranked Ghost Tag matches (ghosttag.ScoreStore) in Redis
// through leaderboard.GhostTagStore, which owns the keys and the
// transaction, and then announces each player's new standing on the
// all-time Ghost Tag board as LeaderboardUpdated, so that other clients'
// boards do not go stale.
//
// That makes the WebSocket service a second producer of LeaderboardUpdated,
// beside the leaderboard service (design doc, section 11). The events carry
// board "ghosttag", and they are the server's statement about a match it
// decided, never anything a client sent.
type gameScores struct {
	store *leaderboard.GhostTagStore
	pub   events.Publisher
	log   *zap.Logger
	// later runs the announcements; go in the service, inline in tests.
	later func(func())
}

// The adapter is what the game asks for.
var _ ghosttag.ScoreStore = gameScores{}

// Settle implements ghosttag.ScoreStore.
func (g gameScores) Settle(ctx context.Context, m ghosttag.Match, score func(map[string]ghosttag.Standing) map[string]ghosttag.Award) (ghosttag.Settlement, error) {
	st, err := g.store.Settle(ctx, leaderboard.GhostTagMatch{RoomID: m.RoomID, Players: m.Players, EndedAt: m.EndedAt},
		func(prior map[string]int) map[string]leaderboard.GhostTagAward {
			standings := make(map[string]ghosttag.Standing, len(prior))
			for id, streak := range prior {
				standings[id] = ghosttag.Standing{WinStreak: streak}
			}
			awards := score(standings)
			out := make(map[string]leaderboard.GhostTagAward, len(awards))
			for id, a := range awards {
				out[id] = leaderboard.GhostTagAward{Delta: a.Delta, WinStreak: a.WinStreak, Win: a.Win, CaptureMs: a.CaptureMs}
			}
			return out
		})
	if err != nil {
		return ghosttag.Settlement{}, err
	}
	res := ghosttag.Settlement{Ranked: st.Ranked}
	if st.Ranked {
		res.Awards = make(map[string]ghosttag.Award, len(st.Awards))
		for id, a := range st.Awards {
			res.Awards[id] = ghosttag.Award{Delta: a.Delta, WinStreak: a.WinStreak, Win: a.Win, CaptureMs: a.CaptureMs}
		}
		players := append([]string(nil), m.Players...)
		g.later(func() { g.announce(players) })
	}
	return res, nil
}

// announce publishes each player's standing on the Ghost Tag board. It is
// best effort, like the leaderboard service's: the board itself is the
// truth and clients can re-read it.
func (g gameScores) announce(players []string) {
	ctx, cancel := context.WithTimeout(context.Background(), announceTimeout)
	defer cancel()
	for _, id := range players {
		entry, err := g.store.GhostTagRank(ctx, id)
		if err != nil {
			g.log.Warn("ghost tag standing unavailable, not announced", zap.String("player_id", id), zap.Error(err))
			continue
		}
		e, err := events.New(events.TypeLeaderboardUpdated, events.LeaderboardUpdatedPayload{
			PlayerID: entry.PlayerID, Score: entry.Score, Rank: entry.Rank, Board: events.BoardGhostTag,
		})
		if err == nil {
			err = g.pub.Publish(ctx, events.TopicLeaderboard, e)
		}
		if err != nil {
			g.log.Warn("failed to publish ghost tag leaderboard event", zap.String("player_id", id), zap.Error(err))
		}
	}
}
