package main

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/ghosttag"
)

// The per-player engagement counters of section 5. They go into the same
// metric as the game's own events, gamemesh_ws_game_events_total, and like
// them carry no player or room label:
//
//	second round   second_round / player_finished
//	D1 return      today's d1_return increase / yesterday's player_day increase
const (
	// eventPlayerFinished: one human player of a completed match.
	eventPlayerFinished = "player_finished"
	// eventSecondRound: a player's new match started within
	// secondRoundWindow of the end of a completed one.
	eventSecondRound = "second_round"
	// eventPlayerDay: a player's first match start of a UTC day.
	eventPlayerDay = "player_day"
	// eventD1Return: a player's first match start of a UTC day, when they
	// also started one on the UTC day before.
	eventD1Return = "d1_return"
)

const (
	// finishedPrefix + player: the player completed a match; the value is
	// when, in Unix milliseconds. It lives secondRoundWindow and is read and
	// deleted when the player's next match starts.
	finishedPrefix = "ghosttag:finished:"
	// activePrefix + YYYY-MM-DD: the set of players who started a match on
	// that UTC day. It lives activeTTL, long enough to be "yesterday".
	activePrefix = "ghosttag:active:"

	secondRoundWindow = 30 * time.Second
	activeTTL         = 3 * 24 * time.Hour

	// turnoutQueue bounds the reports waiting for Redis. A report that does
	// not fit is dropped: telemetry must never hold up a room.
	turnoutQueue = 1024
	// turnoutTimeout bounds the Redis work of one report.
	turnoutTimeout = 2 * time.Second
)

// turnout keeps the short-lived Redis markers the per-player rates need
// (ghosttag.Turnout) and counts their events. The rooms call it on their
// own goroutines; it only queues. One worker (Run) applies the reports in
// the order they came, so a player's completed match is always marked
// before their next one starts is read.
//
// The markers are telemetry: if one cannot be written or read, the match is
// not affected and the counter of that event simply does not move. That is
// why player_finished is counted only once its marker is written: a
// finished player whose second round could never be seen must not count
// against the ratio either.
type turnout struct {
	rdb   *redis.Client
	count ghosttag.Observer
	log   *zap.Logger
	jobs  chan turnoutReport
}

// turnoutReport is one call of the game, waiting for the worker.
type turnoutReport struct {
	completed bool
	players   []string
	at        time.Time
}

var _ ghosttag.Turnout = (*turnout)(nil)

func newTurnout(rdb *redis.Client, count ghosttag.Observer, log *zap.Logger) *turnout {
	return &turnout{rdb: rdb, count: count, log: log, jobs: make(chan turnoutReport, turnoutQueue)}
}

// Started implements ghosttag.Turnout.
func (t *turnout) Started(players []string, at time.Time) {
	t.enqueue(turnoutReport{players: players, at: at})
}

// Completed implements ghosttag.Turnout.
func (t *turnout) Completed(players []string, at time.Time) {
	t.enqueue(turnoutReport{completed: true, players: players, at: at})
}

func (t *turnout) enqueue(r turnoutReport) {
	select {
	case t.jobs <- r:
	default:
		t.log.Warn("ghost tag telemetry queue full, report dropped", zap.Bool("completed", r.completed), zap.Int("players", len(r.players)))
	}
}

// Run applies the reports until ctx is done.
func (t *turnout) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-t.jobs:
			t.apply(ctx, r)
		}
	}
}

func (t *turnout) apply(ctx context.Context, r turnoutReport) {
	ctx, cancel := context.WithTimeout(ctx, turnoutTimeout)
	defer cancel()
	for _, id := range r.players {
		if r.completed {
			t.finished(ctx, id, r.at)
		} else {
			t.started(ctx, id, r.at)
		}
	}
}

// finished marks that id completed a match at at.
func (t *turnout) finished(ctx context.Context, id string, at time.Time) {
	err := t.rdb.Set(ctx, finishedPrefix+id, strconv.FormatInt(at.UnixMilli(), 10), secondRoundWindow).Err()
	if err != nil {
		t.log.Warn("ghost tag telemetry: finished marker not written", zap.Error(err))
		return
	}
	t.count.Count(eventPlayerFinished)
}

// started counts what id's match start at at means: a second round if they
// completed a match within the window before, and the first start of the
// UTC day, a D1 return if they also started one the day before.
func (t *turnout) started(ctx context.Context, id string, at time.Time) {
	switch raw, err := t.rdb.GetDel(ctx, finishedPrefix+id).Result(); {
	case errors.Is(err, redis.Nil):
	case err != nil:
		t.log.Warn("ghost tag telemetry: finished marker not read", zap.Error(err))
	default:
		// The key's TTL is the window on Redis's clock; the value keeps it
		// on the rooms' clock too.
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && at.Sub(time.UnixMilli(ms)) <= secondRoundWindow {
			t.count.Count(eventSecondRound)
		}
	}

	day := at.UTC()
	today, yesterday := activePrefix+day.Format("2006-01-02"), activePrefix+day.AddDate(0, 0, -1).Format("2006-01-02")
	var added *redis.IntCmd
	var before *redis.BoolCmd
	_, err := t.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		added = pipe.SAdd(ctx, today, id)
		pipe.Expire(ctx, today, activeTTL)
		before = pipe.SIsMember(ctx, yesterday, id)
		return nil
	})
	if err != nil {
		t.log.Warn("ghost tag telemetry: day marker not written", zap.Error(err))
	}
	if added.Err() != nil || added.Val() != 1 {
		return // not written, or not the first start of the day
	}
	t.count.Count(eventPlayerDay)
	if before.Err() == nil && before.Val() {
		t.count.Count(eventD1Return)
	}
}
