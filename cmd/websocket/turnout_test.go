package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func newTestTurnout(t *testing.T) (*turnout, *miniredis.Miniredis, *eventCounts) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counts := &eventCounts{n: map[string]int{}}
	return newTurnout(rdb, counts, zap.NewNop()), mr, counts
}

// day0 is late on a UTC day, so that a day later is the next UTC day.
var day0 = time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)

func (tu *turnout) startNow(at time.Time, players ...string) {
	tu.apply(context.Background(), turnoutReport{players: players, at: at})
}

func (tu *turnout) completeNow(at time.Time, players ...string) {
	tu.apply(context.Background(), turnoutReport{completed: true, players: players, at: at})
}

// TestTurnoutSecondRound: every player of a completed match counts as
// finished and is marked for 30 s; a new match within that window is a
// second round, counted once; one later is not.
func TestTurnoutSecondRound(t *testing.T) {
	tu, mr, counts := newTestTurnout(t)
	tu.completeNow(day0, "a", "b")
	assert.Equal(t, 2, counts.get(eventPlayerFinished))
	assert.Equal(t, 30*time.Second, mr.TTL("ghosttag:finished:a"))

	tu.startNow(day0.Add(29*time.Second), "a")
	assert.Equal(t, 1, counts.get(eventSecondRound))
	assert.False(t, mr.Exists("ghosttag:finished:a"), "read and deleted at the start")
	tu.startNow(day0.Add(29*time.Second), "a")
	assert.Equal(t, 1, counts.get(eventSecondRound), "one completed match, one second round")

	tu.startNow(day0.Add(31*time.Second), "b")
	assert.Equal(t, 1, counts.get(eventSecondRound), "31 s after is not a second round")
	assert.False(t, mr.Exists("ghosttag:finished:b"))
}

// TestTurnoutSecondRoundMarkerExpires: the marker is gone once its 30 s are
// over on Redis's clock, whatever the rooms' times say.
func TestTurnoutSecondRoundMarkerExpires(t *testing.T) {
	tu, mr, counts := newTestTurnout(t)
	tu.completeNow(day0, "a")
	mr.FastForward(31 * time.Second)
	tu.startNow(day0.Add(time.Second), "a")
	assert.Zero(t, counts.get(eventSecondRound))
}

// TestTurnoutPlayerDayAndD1Return: a player's first match start of a UTC
// day counts once that day; on the next UTC day it counts again, and as a
// D1 return; after a day without play it is not one.
func TestTurnoutPlayerDayAndD1Return(t *testing.T) {
	tu, mr, counts := newTestTurnout(t)
	tu.startNow(day0, "a", "b")
	tu.startNow(day0.Add(time.Hour), "a") // 21:00, the same UTC day
	assert.Equal(t, 2, counts.get(eventPlayerDay))
	assert.Zero(t, counts.get(eventD1Return), "nobody played the day before")
	assert.Equal(t, 3*24*time.Hour, mr.TTL("ghosttag:active:2026-11-22"))

	tu.startNow(day0.Add(5*time.Hour), "a") // 01:00 on 2026-11-23
	assert.Equal(t, 3, counts.get(eventPlayerDay))
	assert.Equal(t, 1, counts.get(eventD1Return))
	tu.startNow(day0.Add(6*time.Hour), "a")
	assert.Equal(t, 1, counts.get(eventD1Return), "once a day")

	tu.startNow(day0.Add(52*time.Hour), "b") // 2026-11-25: skipped a day
	assert.Equal(t, 4, counts.get(eventPlayerDay))
	assert.Equal(t, 1, counts.get(eventD1Return))
}

// TestTurnoutDaysAreUTC: the day is the UTC day of the room's time, not
// the day of its time zone.
func TestTurnoutDaysAreUTC(t *testing.T) {
	tu, mr, _ := newTestTurnout(t)
	tu.startNow(time.Date(2026, 11, 22, 21, 0, 0, 0, time.FixedZone("UTC-5", -5*3600)), "a")
	ok, err := mr.SIsMember("ghosttag:active:2026-11-23", "a")
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestTurnoutWithoutRedisCountsNothing: with the markers unwritable no
// counter moves, and the calls still return.
func TestTurnoutWithoutRedisCountsNothing(t *testing.T) {
	tu, mr, counts := newTestTurnout(t)
	mr.Close()
	tu.completeNow(day0, "a")
	tu.startNow(day0.Add(time.Second), "a")
	for _, e := range []string{eventPlayerFinished, eventSecondRound, eventPlayerDay, eventD1Return} {
		assert.Zero(t, counts.get(e), e)
	}
}

// TestTurnoutNeverBlocksTheRoom: with the worker stalled, a full queue
// drops a report and logs it instead of waiting.
func TestTurnoutNeverBlocksTheRoom(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	tu := newTurnout(nil, &eventCounts{n: map[string]int{}}, zap.New(core))
	for range turnoutQueue {
		tu.Started([]string{"a"}, day0)
	}
	require.Zero(t, logs.Len())
	tu.Completed([]string{"a"}, day0) // returns: the test would hang otherwise
	assert.Equal(t, 1, logs.FilterMessage("ghost tag telemetry queue full, report dropped").Len())
}

// TestTurnoutWorkerKeepsTheOrder: reports go through the worker in the
// order the rooms made them, so a completion is marked before the next
// start reads it.
func TestTurnoutWorkerKeepsTheOrder(t *testing.T) {
	tu, _, counts := newTestTurnout(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tu.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for i := range 20 {
		at := day0.Add(time.Duration(i) * time.Second)
		tu.Completed([]string{"a"}, at)
		tu.Started([]string{"a"}, at.Add(time.Millisecond))
	}
	deadline := time.After(frameTimeout)
	for counts.get(eventPlayerDay) < 1 || counts.get(eventSecondRound) < 20 {
		select {
		case <-deadline:
			t.Fatalf("second rounds: %d of 20", counts.get(eventSecondRound))
		case <-time.After(time.Millisecond):
		}
	}
	assert.Equal(t, 20, counts.get(eventPlayerFinished))
}
