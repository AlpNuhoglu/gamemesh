package leaderboard

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// removeDailyDays is how many daily Ghost Tag boards, counting back from
// today, a deleted player is taken out of. A daily board expires eight days
// after its last write, so nine covers every board that can still exist.
const removeDailyDays = 9

// PlayerRemover takes a deleted player out of everything this package keeps
// about them: the global board, the Ghost Tag boards and the stats hash.
//
// The player service owns the account and deletes the row; this service owns
// these keys. The two are joined by the PlayerDeleted event, which the player
// service writes in the transaction that deletes the row, so the removal
// cannot be forgotten, only delayed.
//
// What is left on purpose: ghosttag:settled:{room} and ghosttag:pair:* name
// player ids but are not shown anywhere, and expire on their own within a
// week.
type PlayerRemover struct {
	rdb *redis.Client
	log *zap.Logger
	now func() time.Time
}

// NewPlayerRemover wires the remover.
func NewPlayerRemover(rdb *redis.Client, log *zap.Logger) *PlayerRemover {
	return &PlayerRemover{rdb: rdb, log: log, now: time.Now}
}

// Remove deletes the player's entries. Removing a player who has none is not
// an error, so a redelivered event is harmless.
func (r *PlayerRemover) Remove(ctx context.Context, playerID string) error {
	today := r.now()
	_, err := r.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.ZRem(ctx, Key, playerID)
		p.ZRem(ctx, GhostTagAllTimeKey, playerID)
		for back := 0; back < removeDailyDays; back++ {
			p.ZRem(ctx, GhostTagDailyKey(today.AddDate(0, 0, -back)), playerID)
		}
		p.Del(ctx, GhostTagStatsKey(playerID))
		return nil
	})
	return err
}

// Handle is the events.Handler of the player topic: it acts on PlayerDeleted
// and ignores everything else. A payload that cannot be read is dropped (nil,
// so it is not redelivered forever); a Redis failure is returned, so the
// event comes again.
func (r *PlayerRemover) Handle(ctx context.Context, e events.Event) error {
	if e.Type != events.TypePlayerDeleted {
		return nil
	}
	var payload events.PlayerDeletedPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.PlayerID == "" {
		r.log.Warn("malformed PlayerDeleted payload", zap.Error(err))
		return nil
	}
	if err := r.Remove(ctx, payload.PlayerID); err != nil {
		r.log.Warn("failed to remove a deleted player from the leaderboards", zap.Error(err), zap.String("player_id", payload.PlayerID))
		return err
	}
	r.log.Info("deleted player removed from the leaderboards", zap.String("player_id", payload.PlayerID))
	return nil
}
