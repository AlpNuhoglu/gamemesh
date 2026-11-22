package ghosttag

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// turnoutCall is one call the room made on its Turnout.
type turnoutCall struct {
	completed bool
	players   []string
	at        time.Time
}

// turnoutLog records the calls; it is safe for the session's goroutine and
// the test's.
type turnoutLog struct {
	mu    sync.Mutex
	calls []turnoutCall
}

func (l *turnoutLog) Started(players []string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, turnoutCall{players: players, at: at})
}

func (l *turnoutLog) Completed(players []string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, turnoutCall{completed: true, players: players, at: at})
}

func (l *turnoutLog) all() []turnoutCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]turnoutCall(nil), l.calls...)
}

func withTurnout(h *harness) *turnoutLog {
	l := &turnoutLog{}
	h.r.turnout = l
	return l
}

// TestTurnoutOfACompletedMatch: the players are reported as starting when
// the room leaves WAITING and as completing when the second half ends, each
// at the room's time of that transition, and exactly when the two counters
// move.
func TestTurnoutOfACompletedMatch(t *testing.T) {
	h := twoPlayers(t)
	l := withTurnout(h)
	h.r.setPresence(at(time.Second), "a", true)
	assert.Empty(t, l.all(), "WAITING is not a start")
	h.r.setPresence(at(2*time.Second), "b", true)
	require.Equal(t, []turnoutCall{{players: []string{"a", "b"}, at: at(2 * time.Second)}}, l.all())
	assert.Equal(t, 1, h.obs[EventMatchStarted])

	start := at(2*time.Second + h.cfg.TCountdown)
	h.r.tick(start)
	h.r.tick(start.Add(h.cfg.THalf))
	require.Len(t, l.all(), 1, "the end of the first half is not a completion")
	h.r.tick(start.Add(h.cfg.THalf + h.cfg.TRoleSwap))
	end := start.Add(2*h.cfg.THalf + h.cfg.TRoleSwap)
	h.r.tick(end.Add(ms(7))) // a late tick: the half ended at its deadline
	require.True(t, h.r.finished())

	calls := l.all()
	require.Len(t, calls, 2)
	assert.Equal(t, turnoutCall{completed: true, players: []string{"a", "b"}, at: end}, calls[1])
	assert.Equal(t, 1, h.obs[EventMatchCompleted])
}

// TestTurnoutLeavesOutTheBot: in a bot match only the human is reported,
// as starting and as completing.
func TestTurnoutLeavesOutTheBot(t *testing.T) {
	h := newHarness(t, DefaultConfig(), "room-b", []string{"human"}, "bot")
	l := withTurnout(h)
	h.r.setPresence(at(0), "human", true)
	h.r.tick(at(h.cfg.TCountdown))
	h.r.tick(at(h.cfg.TCountdown + h.cfg.THalf))
	h.r.tick(at(h.cfg.TCountdown + h.cfg.THalf + h.cfg.TRoleSwap))
	h.r.tick(at(h.cfg.TCountdown + 2*h.cfg.THalf + h.cfg.TRoleSwap))
	require.True(t, h.r.finished())
	require.Equal(t, outcomeCompleted, h.r.result.outcome)

	calls := l.all()
	require.Len(t, calls, 2)
	assert.Equal(t, []string{"human"}, calls[0].players)
	assert.False(t, calls[0].completed)
	assert.Equal(t, []string{"human"}, calls[1].players)
	assert.True(t, calls[1].completed)
}

// TestTurnoutOfMatchesNotCompleted: a forfeit and a void match started but
// did not complete; a match cancelled in WAITING never started.
func TestTurnoutOfMatchesNotCompleted(t *testing.T) {
	forfeit := twoPlayers(t)
	l := withTurnout(forfeit)
	start := forfeit.started()
	forfeit.r.setPresence(start.Add(time.Second), "b", false)
	forfeit.r.tick(start.Add(time.Second + forfeit.cfg.TGrace))
	require.Equal(t, outcomeForfeit, forfeit.r.result.outcome)
	calls := l.all()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].completed)

	void := twoPlayers(t)
	l = withTurnout(void)
	start = void.started()
	void.r.setPresence(start.Add(time.Second), "a", false)
	void.r.setPresence(start.Add(time.Second), "b", false)
	void.r.tick(start.Add(time.Second + void.cfg.TGrace))
	require.Equal(t, outcomeVoid, void.r.result.outcome)
	calls = l.all()
	require.Len(t, calls, 1)
	assert.False(t, calls[0].completed)

	cancelled := twoPlayers(t)
	l = withTurnout(cancelled)
	cancelled.r.setPresence(at(time.Second), "a", true)
	cancelled.r.tick(at(cancelled.cfg.TJoin))
	require.Equal(t, outcomeCancelled, cancelled.r.result.outcome)
	assert.Empty(t, l.all())
}

// TestSessionReportsTurnout: Spec.Turnout is the one the room tells.
func TestSessionReportsTurnout(t *testing.T) {
	l := &turnoutLog{}
	spec := pvp()
	spec.Turnout = l
	rig := newRig(t, fastConfig(), spec, newFakeTransport("a", "b"), nil)
	rig.run(context.Background())
	rig.waitDone(t)

	calls := l.all()
	require.Len(t, calls, 2)
	assert.ElementsMatch(t, []string{"a", "b"}, calls[0].players)
	assert.False(t, calls[0].completed)
	assert.True(t, calls[1].completed)
	assert.False(t, calls[1].at.Before(calls[0].at))
}
