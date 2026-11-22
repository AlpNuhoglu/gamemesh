package leaderboard

// Ghost Tag's leaderboards and player stats (design doc, section 4).
//
//	GhostTagAllTimeKey               sorted set, the all-time board
//	lb:ghosttag:daily:YYYY-MM-DD     sorted set, one board per UTC day
//	player:stats:{player_id}         hash: name, wins, win_streak,
//	                                 fastest_capture_ms
//	ghosttag:settled:{room}          the settlement of a room, so a retry of
//	                                 the same room writes nothing twice
//	ghosttag:pair:{day}:{a}:{b}      the two players already settled a match
//	                                 on that UTC day
//
// The WebSocket service settles a finished match through this store, in one
// optimistic transaction: the streaks the score depends on are read in the
// same transaction that writes the points, so two matches of one player
// finishing at once cannot both build on the same streak.
//
// The scoring rule is the game's (internal/ghosttag); this file only knows
// the keys and the transaction, and takes the rule as a function.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// GhostTagAllTimeKey is the all-time Ghost Tag board.
const GhostTagAllTimeKey = "lb:ghosttag:alltime" //nolint:misspell // the key's name is the design doc's (section 4)

const (
	ghostDailyPrefix   = "lb:ghosttag:daily:"
	ghostStatsPrefix   = "player:stats:"
	ghostSettledPrefix = "ghosttag:settled:"
	ghostPairPrefix    = "ghosttag:pair:"

	statName      = "name"
	statWins      = "wins"
	statStreak    = "win_streak"
	statFastestMs = "fastest_capture_ms"

	// ghostSettleRetries bounds the optimistic transaction's retries when a
	// watched key changed under it: another settlement of one of the same
	// players, or of the same room.
	ghostSettleRetries = 5
)

// GhostTagMatch is a finished human-vs-human match to settle.
type GhostTagMatch struct {
	RoomID  string
	Players []string
	// Names are the players' display names by ID. A ranked settlement
	// writes each into the player's stats, for the board; a player absent
	// here keeps the name stored before.
	Names   map[string]string
	EndedAt time.Time
}

// GhostTagAward is what a match does to one player.
type GhostTagAward struct {
	Delta     int  `json:"delta"`
	WinStreak int  `json:"win_streak"`
	Win       bool `json:"win"`
	// CaptureMs is the player's capture time as hunter, 0 for none; the
	// store keeps the fastest.
	CaptureMs int `json:"capture_ms"`
}

// GhostTagSettlement is the outcome of a settlement.
type GhostTagSettlement struct {
	// Ranked is false when the two players had already settled a match on
	// that UTC day: nothing was written.
	Ranked bool                     `json:"ranked"`
	Awards map[string]GhostTagAward `json:"awards,omitempty"`
}

// GhostTagStore writes Ghost Tag results to Redis.
type GhostTagStore struct {
	rdb *redis.Client
	// dailyTTL is how long a day's board is kept; settledTTL how long a
	// room's settlement is remembered for retries.
	dailyTTL, settledTTL time.Duration
}

// NewGhostTagStore wraps an existing Redis client. A day's board is kept for
// 8 days, a room's settlement for 7.
func NewGhostTagStore(rdb *redis.Client) *GhostTagStore {
	return &GhostTagStore{rdb: rdb, dailyTTL: 8 * 24 * time.Hour, settledTTL: 7 * 24 * time.Hour}
}

// GhostTagDailyKey is the board of the UTC day of t.
func GhostTagDailyKey(t time.Time) string { return ghostDailyPrefix + ghostDay(t) }

// GhostTagStatsKey is a player's stats hash.
func GhostTagStatsKey(playerID string) string { return ghostStatsPrefix + playerID }

func ghostDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// ghostPairKey names the two players of a match on its UTC day, in a fixed
// order so that it does not depend on who is listed first.
func ghostPairKey(m GhostTagMatch) string {
	ids := append([]string(nil), m.Players...)
	sort.Strings(ids)
	k := ghostPairPrefix + ghostDay(m.EndedAt)
	for _, id := range ids {
		k += ":" + id
	}
	return k
}

// ErrGhostTagContention is returned when the transaction kept losing to
// concurrent writes of the same keys; nothing was written.
var ErrGhostTagContention = errors.New("ghost tag settlement: too much contention, nothing written")

// Settle applies the match once and atomically. It reads every player's
// current win streak, calls score with them, and writes what score returns:
// the delta to the all-time and the day's board, and the name, streak,
// wins and fastest capture to the player's stats.
//
// A second Settle of the same room writes nothing and returns the first
// one's settlement, without calling score. If the same two players already
// settled a match on the UTC day of m.EndedAt, nothing is written and the
// settlement is unranked; that is remembered for the room too.
//
// score must be pure: a transaction that loses to a concurrent write is
// retried, and score is called again with what is then in Redis. On an
// error nothing was written.
func (s *GhostTagStore) Settle(ctx context.Context, m GhostTagMatch, score func(prior map[string]int) map[string]GhostTagAward) (GhostTagSettlement, error) {
	if m.RoomID == "" || len(m.Players) == 0 {
		return GhostTagSettlement{}, errors.New("ghost tag settlement: no room or no players")
	}
	settledKey := ghostSettledPrefix + m.RoomID
	pairKey := ghostPairKey(m)
	watched := []string{settledKey, pairKey}
	for _, id := range m.Players {
		watched = append(watched, GhostTagStatsKey(id))
	}

	var out GhostTagSettlement
	txf := func(tx *redis.Tx) error {
		// 1. This room was settled already: the same answer, no write.
		raw, err := tx.Get(ctx, settledKey).Bytes()
		switch {
		case err == nil:
			out = GhostTagSettlement{}
			return json.Unmarshal(raw, &out)
		case !errors.Is(err, redis.Nil):
			return err
		}

		// 2. The same two players already settled a match today: unranked.
		paired, err := tx.Exists(ctx, pairKey).Result()
		if err != nil {
			return err
		}
		if paired > 0 {
			out = GhostTagSettlement{Ranked: false}
			return s.commit(ctx, tx, settledKey, out, nil)
		}

		// 3. Read the standings, score, write.
		prior := make(map[string]int, len(m.Players))
		fastest := make(map[string]int, len(m.Players))
		for _, id := range m.Players {
			vals, err := tx.HMGet(ctx, GhostTagStatsKey(id), statStreak, statFastestMs).Result()
			if err != nil {
				return err
			}
			prior[id] = hashInt(vals[0])
			fastest[id] = hashInt(vals[1])
		}
		awards := score(prior)
		out = GhostTagSettlement{Ranked: true, Awards: awards}
		day := GhostTagDailyKey(m.EndedAt)
		return s.commit(ctx, tx, settledKey, out, func(pipe redis.Pipeliner) {
			for id, a := range awards {
				pipe.ZIncrBy(ctx, GhostTagAllTimeKey, float64(a.Delta), id)
				pipe.ZIncrBy(ctx, day, float64(a.Delta), id)
				stats := GhostTagStatsKey(id)
				if name := m.Names[id]; name != "" {
					pipe.HSet(ctx, stats, statName, name)
				}
				pipe.HSet(ctx, stats, statStreak, a.WinStreak)
				if a.Win {
					pipe.HIncrBy(ctx, stats, statWins, 1)
				}
				if a.CaptureMs > 0 && (fastest[id] == 0 || a.CaptureMs < fastest[id]) {
					pipe.HSet(ctx, stats, statFastestMs, a.CaptureMs)
				}
			}
			pipe.Expire(ctx, day, s.dailyTTL)
			// The pair key only has to outlive its day.
			pipe.Set(ctx, pairKey, m.RoomID, 48*time.Hour)
		})
	}

	for range ghostSettleRetries {
		err := s.rdb.Watch(ctx, txf, watched...)
		if errors.Is(err, redis.TxFailedErr) {
			continue // a watched key changed: read again and retry
		}
		if err != nil {
			return GhostTagSettlement{}, fmt.Errorf("ghost tag settlement of room %s: %w", m.RoomID, err)
		}
		return out, nil
	}
	return GhostTagSettlement{}, ErrGhostTagContention
}

// commit writes the settlement record and whatever write adds, in one
// MULTI/EXEC that fails if a watched key changed since it was read.
func (s *GhostTagStore) commit(ctx context.Context, tx *redis.Tx, settledKey string, out GhostTagSettlement, write func(redis.Pipeliner)) error {
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		if write != nil {
			write(pipe)
		}
		pipe.Set(ctx, settledKey, raw, s.settledTTL)
		return nil
	})
	return err
}

// GhostTagRank is a player's entry on the all-time Ghost Tag board.
func (s *GhostTagStore) GhostTagRank(ctx context.Context, playerID string) (Entry, error) {
	rank, err := s.rdb.ZRevRank(ctx, GhostTagAllTimeKey, playerID).Result()
	if errors.Is(err, redis.Nil) {
		return Entry{}, ErrPlayerNotRanked
	}
	if err != nil {
		return Entry{}, err
	}
	score, err := s.rdb.ZScore(ctx, GhostTagAllTimeKey, playerID).Result()
	if err != nil {
		return Entry{}, err
	}
	return Entry{PlayerID: playerID, Score: score, Rank: rank + 1}, nil
}

// GhostTagRow is one row of a Ghost Tag board as the public read endpoint
// shows it: a name, a score and a rank, and nothing else. No player ID, no
// e-mail, no date (design doc, section 4).
type GhostTagRow struct {
	Name  string  `json:"name"`
	Score float64 `json:"score"`
	Rank  int64   `json:"rank"`
}

// GhostTagUnnamed is the name a row shows for a player whose stats hold
// none: one whose matches were all settled before names were stored.
const GhostTagUnnamed = "Player"

// GhostTagTop returns the limit best rows of a Ghost Tag board: the
// all-time board, or the day's when daily is set (the board of the UTC day
// of day). Ranks are 1-based. A board nobody has scored on is empty.
func (s *GhostTagStore) GhostTagTop(ctx context.Context, daily bool, day time.Time, limit int64) ([]GhostTagRow, error) {
	key := GhostTagAllTimeKey
	if daily {
		key = GhostTagDailyKey(day)
	}
	zs, err := s.rdb.ZRevRangeWithScores(ctx, key, 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	rows := make([]GhostTagRow, len(zs))
	if len(zs) == 0 {
		return rows, nil
	}
	// One round trip for the names. A player without one answers
	// redis.Nil, which is not an error here.
	names := make([]*redis.StringCmd, len(zs))
	_, _ = s.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, z := range zs {
			id, _ := z.Member.(string)
			names[i] = pipe.HGet(ctx, GhostTagStatsKey(id), statName)
		}
		return nil
	})
	for i, z := range zs {
		name, err := names[i].Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}
		if name == "" {
			name = GhostTagUnnamed
		}
		rows[i] = GhostTagRow{Name: name, Score: z.Score, Rank: int64(i) + 1}
	}
	return rows, nil
}

// hashInt reads an integer field of HMGET, 0 for a missing or unreadable one.
func hashInt(v any) int {
	str, ok := v.(string)
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(str)
	if err != nil {
		return 0
	}
	return n
}
