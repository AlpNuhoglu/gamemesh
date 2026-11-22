package player

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// sweepFixture is a service whose log is captured and whose table holds
// accounts of every kind the rules must tell apart.
type sweepFixture struct {
	*guestFixture
	logs *observer.ObservedLogs
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.InfoLevel)
	f := newGuestFixture(WithGuests(testGuestTTL))
	f.svc.log = zap.New(core)
	return &sweepFixture{guestFixture: f, logs: logs}
}

// add puts an account straight into the table.
func (f *sweepFixture) add(username, email string, guest bool, created time.Time, lastSeen *time.Time) uuid.UUID {
	id := uuid.New()
	f.repo.players[id] = &Player{
		ID: id, Username: username, Email: email, IsGuest: guest,
		CreatedAt: created, LastSeenAt: lastSeen,
	}
	return id
}

const testRetention = 30 * 24 * time.Hour

func (f *sweepFixture) has(id uuid.UUID) bool {
	_, ok := f.repo.players[id]
	return ok
}

func TestSweep(t *testing.T) {
	f := newSweepFixture(t)
	ctx := context.Background()
	ago := func(d time.Duration) time.Time { return f.now.Add(-d) }
	seen := func(d time.Duration) *time.Time { at := ago(d); return &at }
	day := 24 * time.Hour

	idle := f.add("QuietOwl0001", "a@guest.invalid", true, ago(90*day), seen(31*day))
	active := f.add("SwiftFox0002", "b@guest.invalid", true, ago(90*day), seen(29*day))
	oldBench := f.add("bench7f3a1", "bench_7f3a_1@latencybench.invalid", false, ago(2*day), nil)
	newBench := f.add("bench9c2b1", "bench_9c2b_1@latencybench.invalid", false, ago(time.Hour), nil)
	member := f.add("alice", "alice@example.com", false, ago(400*day), nil)
	// Alice has one session; the idle guest has a stale one too.
	f.sessions.saved["jti-idle"] = idle.String()
	f.sessions.saved["jti-member"] = member.String()

	out, err := f.svc.Sweep(ctx, testRetention, 500)
	require.NoError(t, err)
	assert.Equal(t, SweepResult{Guests: 1, Bench: 1}, out)

	assert.False(t, f.has(idle), "a guest idle for 31 days was kept")
	assert.True(t, f.has(active), "a guest seen 29 days ago was deleted")
	assert.False(t, f.has(oldBench), "a two-day-old bench account was kept")
	assert.True(t, f.has(newBench), "the account of a measurement that may be running was deleted")
	assert.True(t, f.has(member), "a registered player was deleted")

	// The cutoffs the repository was given.
	require.Len(t, f.repo.sweeps, 2)
	assert.Equal(t, sweepCall{rule: SweepInactiveGuests, cutoff: f.now.Add(-testRetention), limit: 500}, f.repo.sweeps[0])
	assert.Equal(t, sweepCall{rule: SweepBenchAccounts, cutoff: f.now.Add(-24 * time.Hour), limit: 499}, f.repo.sweeps[1])

	// Every deleted account is announced and has no session left.
	assert.Len(t, eventsOfType(t, f.repo, events.TypePlayerDeleted), 2)
	assert.Equal(t, map[string]string{"jti-member": member.String()}, f.sessions.saved)

	// The run says what it did.
	entries := f.logs.FilterMessage("account sweep").All()
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{
		"guests_deleted": int64(1), "bench_deleted": int64(1), "max_per_run": int64(500), "capped": false,
	}, entries[0].ContextMap())
}

// The limit is for the whole run: what the first rule deletes comes off what
// the second may.
func TestSweepIsCappedPerRun(t *testing.T) {
	f := newSweepFixture(t)
	ctx := context.Background()
	day := 24 * time.Hour
	for i := 0; i < 4; i++ {
		at := f.now.Add(-time.Duration(40+i) * day)
		f.add("QuietOwl000"+string(rune('0'+i)), string(rune('a'+i))+"@guest.invalid", true, at, &at)
	}
	for i := 0; i < 4; i++ {
		f.add("bench"+string(rune('a'+i)), "bench_x_"+string(rune('0'+i))+"@latencybench.invalid", false, f.now.Add(-3*day), nil)
	}

	out, err := f.svc.Sweep(ctx, testRetention, 5)
	require.NoError(t, err)
	assert.Equal(t, SweepResult{Guests: 4, Bench: 1, Capped: true}, out)
	assert.Len(t, f.repo.players, 3, "one run deleted more than its limit")
	require.Len(t, f.repo.sweeps, 2)
	assert.Equal(t, 5, f.repo.sweeps[0].limit)
	assert.Equal(t, 1, f.repo.sweeps[1].limit, "the second rule was given more than the first left")

	entries := f.logs.FilterMessage("account sweep").All()
	require.Len(t, entries, 1)
	assert.Equal(t, true, entries[0].ContextMap()["capped"])

	// The first rule alone can use the limit up; the second is then not run.
	f2 := newSweepFixture(t)
	for i := 0; i < 4; i++ {
		at := f2.now.Add(-time.Duration(40+i) * day)
		f2.add("QuietOwl000"+string(rune('0'+i)), string(rune('a'+i))+"@guest.invalid", true, at, &at)
	}
	f2.add("bencha", "bench_x_0@latencybench.invalid", false, f2.now.Add(-3*day), nil)
	out, err = f2.svc.Sweep(ctx, testRetention, 3)
	require.NoError(t, err)
	assert.Equal(t, SweepResult{Guests: 3, Capped: true}, out)
	assert.Len(t, f2.repo.sweeps, 1)
	assert.Len(t, f2.repo.players, 2)
}

// A limit of zero is "delete nothing", never "no limit".
func TestSweepWithNoBudgetDeletesNothing(t *testing.T) {
	for _, limit := range []int{0, -1} {
		f := newSweepFixture(t)
		at := f.now.Add(-90 * 24 * time.Hour)
		f.add("QuietOwl0001", "a@guest.invalid", true, at, &at)

		out, err := f.svc.Sweep(context.Background(), testRetention, limit)
		require.NoError(t, err)
		assert.Equal(t, SweepResult{}, out)
		assert.Len(t, f.repo.players, 1)
		assert.Empty(t, f.repo.sweeps, "the repository was asked to delete with a limit of %d", limit)
	}
}

// A run with nothing to delete still logs, so a sweep that stopped running
// can be told from one that had nothing to do.
func TestSweepLogsARunThatDeletedNothing(t *testing.T) {
	f := newSweepFixture(t)
	_, err := f.svc.Sweep(context.Background(), testRetention, 500)
	require.NoError(t, err)
	entries := f.logs.FilterMessage("account sweep").All()
	require.Len(t, entries, 1)
	assert.Equal(t, int64(0), entries[0].ContextMap()["guests_deleted"])
	assert.Equal(t, int64(0), entries[0].ContextMap()["bench_deleted"])
}

func TestSweepReportsAFailure(t *testing.T) {
	f := newSweepFixture(t)
	boom := errors.New("database down")
	f.repo.sweepErr = boom

	_, err := f.svc.Sweep(context.Background(), testRetention, 500)
	require.ErrorIs(t, err, boom)
	assert.Len(t, f.repo.sweeps, 1, "the second rule ran after the first failed")
	assert.Len(t, f.logs.FilterMessage("account sweep failed").All(), 1)
	assert.Empty(t, f.logs.FilterMessage("account sweep").All())
}

// The sweep runs when the service starts, not one interval later: a service
// redeployed more often than the interval would never sweep.
func TestRunSweepRunsAtStartAndStops(t *testing.T) {
	f := newSweepFixture(t)
	f.repo.swept = make(chan sweepCall, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.RunSweep(ctx, time.Hour, testRetention, 500)
	}()

	for _, rule := range []SweepRule{SweepInactiveGuests, SweepBenchAccounts} {
		select {
		case call := <-f.repo.swept:
			assert.Equal(t, rule, call.rule)
		case <-time.After(5 * time.Second):
			t.Fatal("the sweep did not run at start")
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep loop did not stop")
	}
}
