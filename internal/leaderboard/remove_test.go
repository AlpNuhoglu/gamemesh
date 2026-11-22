package leaderboard

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

	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

const (
	gone = "11111111-1111-1111-1111-111111111111"
	kept = "22222222-2222-2222-2222-222222222222"
)

// newRemover seeds every key the package keeps for two players, on a fixed
// day, with boards of today, of six days ago and of eight days ago (the
// oldest that can still exist).
func newRemover(t *testing.T) (*PlayerRemover, *miniredis.Miniredis, []string) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Date(2026, 11, 22, 0, 30, 0, 0, time.UTC)
	r := &PlayerRemover{rdb: rdb, log: zap.NewNop(), now: func() time.Time { return now }}

	boards := []string{
		Key,
		GhostTagAllTimeKey,
		GhostTagDailyKey(now),
		GhostTagDailyKey(now.AddDate(0, 0, -6)),
		GhostTagDailyKey(now.AddDate(0, 0, -8)),
	}
	for _, board := range boards {
		for _, player := range []string{gone, kept} {
			_, err := mr.ZAdd(board, 100, player)
			require.NoError(t, err)
		}
	}
	for _, player := range []string{gone, kept} {
		mr.HSet(GhostTagStatsKey(player), "wins", "3", "name", "SwiftFox4821")
	}
	return r, mr, boards
}

func assertRemoved(t *testing.T, mr *miniredis.Miniredis, boards []string, removed bool) {
	t.Helper()
	for _, board := range boards {
		members, err := mr.ZMembers(board)
		require.NoError(t, err)
		if removed {
			assert.Equal(t, []string{kept}, members, board)
		} else {
			assert.ElementsMatch(t, []string{gone, kept}, members, board)
		}
	}
	assert.Equal(t, !removed, mr.Exists(GhostTagStatsKey(gone)), "the deleted player's stats")
	assert.True(t, mr.Exists(GhostTagStatsKey(kept)), "another player's stats were deleted")
}

func deletedEvent(t *testing.T, payload string) events.Event {
	t.Helper()
	return events.Event{Type: events.TypePlayerDeleted, Payload: json.RawMessage(payload)}
}

func TestRemoveDeletedPlayer(t *testing.T) {
	r, mr, boards := newRemover(t)
	ctx := context.Background()

	require.NoError(t, r.Handle(ctx, deletedEvent(t, `{"player_id":"`+gone+`"}`)))
	assertRemoved(t, mr, boards, true)

	// The same event again (at-least-once delivery) changes nothing.
	require.NoError(t, r.Handle(ctx, deletedEvent(t, `{"player_id":"`+gone+`"}`)))
	assertRemoved(t, mr, boards, true)
}

func TestRemoveIgnoresOtherEvents(t *testing.T) {
	r, mr, boards := newRemover(t)
	ctx := context.Background()

	// Another event that names the player is not a deletion.
	e := events.Event{Type: events.TypePlayerUpdated, Payload: json.RawMessage(`{"player_id":"` + gone + `"}`)}
	require.NoError(t, r.Handle(ctx, e))
	// A deletion that names nobody, or cannot be read, is dropped, not retried.
	require.NoError(t, r.Handle(ctx, deletedEvent(t, `{}`)))
	require.NoError(t, r.Handle(ctx, deletedEvent(t, `not json`)))

	assertRemoved(t, mr, boards, false)
}

// A removal that could not reach Redis must be reported, so the event is
// delivered again instead of leaving the player on the boards for good.
func TestRemoveReportsAFailure(t *testing.T) {
	r, mr, _ := newRemover(t)
	mr.Close()
	assert.Error(t, r.Handle(context.Background(), deletedEvent(t, `{"player_id":"`+gone+`"}`)))
}
