package ghosttag

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The match total of the grace (section 2, "Maç toplamı"): per drop at most
// TGrace (5 s), over the match at most TGraceTotal (15 s) per player. All on
// a fake clock.

// dropFor drops id at from and brings them back after away, which must be
// inside their deadline; it returns the time of the return.
func (h *harness) dropFor(id string, from time.Time, away time.Duration) time.Time {
	h.t.Helper()
	h.r.setPresence(from, id, false)
	require.Equal(h.t, stateGrace, h.r.state)
	back := from.Add(away)
	h.r.setPresence(back, id, true)
	require.False(h.t, h.r.finished(), "%s came back in time", id)
	return back
}

// TestGraceTotalShortensTheFourthDrop: three drops of 4 s use 12 of the
// 15 s; the fourth drop's deadline is 3 s away, not 5.
func TestGraceTotalShortensTheFourthDrop(t *testing.T) {
	h := twoPlayers(t)
	now := h.started()
	for i := 0; i < 3; i++ {
		now = h.dropFor("b", now.Add(time.Second), 4*time.Second)
	}
	require.Equal(t, 12*time.Second, h.r.byID["b"].graceUsed)

	drop := now.Add(time.Second)
	h.r.setPresence(drop, "b", false)
	h.r.tick(drop.Add(3*time.Second - time.Millisecond))
	require.Equal(t, stateGrace, h.r.state)
	h.r.tick(drop.Add(3 * time.Second))

	require.True(t, h.r.finished())
	assert.Equal(t, "b", h.r.result.leaver)
	assert.Equal(t, drop.Add(3*time.Second), h.r.endedAt)
}

// TestGraceTotalUsedUpForfeitsAtTheDrop: a player with nothing left of the
// total forfeits at the instant of their next drop, before any tick.
func TestGraceTotalUsedUpForfeitsAtTheDrop(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	h.r.byID["b"].graceUsed = h.cfg.TGraceTotal

	drop := start.Add(time.Second)
	h.r.setPresence(drop, "b", false)

	require.True(t, h.r.finished())
	assert.Equal(t, outcomeForfeit, h.r.result.outcome)
	assert.Equal(t, "b", h.r.result.leaver)
	assert.Equal(t, drop, h.r.endedAt)
}

// TestGraceTotalIsPerPlayer: B's 12 s do not shorten A's grace; A still gets
// the full 5 s on their first drop.
func TestGraceTotalIsPerPlayer(t *testing.T) {
	h := twoPlayers(t)
	now := h.started()
	for i := 0; i < 3; i++ {
		now = h.dropFor("b", now.Add(time.Second), 4*time.Second)
	}

	drop := now.Add(time.Second)
	h.r.setPresence(drop, "a", false)
	h.r.tick(drop.Add(h.cfg.TGrace - time.Millisecond))
	require.Equal(t, stateGrace, h.r.state)
	assert.Zero(t, h.r.byID["a"].graceUsed)
	h.r.tick(drop.Add(h.cfg.TGrace))

	require.True(t, h.r.finished())
	assert.Equal(t, "a", h.r.result.leaver)
}

// TestGraceTotalCarriesOverTheRoleSwap: the total is per match. B uses 12 s
// in the first half and has 3 s left in the second.
func TestGraceTotalCarriesOverTheRoleSwap(t *testing.T) {
	h := twoPlayers(t)
	now := h.started()
	for i := 0; i < 3; i++ {
		now = h.dropFor("b", now.Add(time.Second), 4*time.Second)
	}
	// The half clock stopped during each grace: 3 s of play so far, so the
	// half ends 42 s after the last return.
	end := now.Add(h.cfg.THalf - 3*time.Second)
	h.r.tick(end)
	require.Equal(t, stateRoleSwap, h.r.state)
	half2 := end.Add(h.cfg.TRoleSwap)
	h.r.tick(half2)
	require.Equal(t, stateHalf2, h.r.state)

	drop := half2.Add(time.Second)
	h.r.setPresence(drop, "b", false)
	h.r.tick(drop.Add(3 * time.Second))

	require.True(t, h.r.finished())
	assert.Equal(t, "b", h.r.result.leaver)
	assert.Equal(t, drop.Add(3*time.Second), h.r.endedAt)
}

// TestGraceTotalCountdownDrops: a drop during the countdown that is over
// before it ends costs nothing; a player found gone at its end is charged
// from the grace entry, not from the drop.
func TestGraceTotalCountdownDrops(t *testing.T) {
	h := twoPlayers(t)
	h.r.setPresence(at(0), "a", true)
	h.r.setPresence(at(0), "b", true)
	h.r.setPresence(at(ms(500)), "b", false)
	h.r.setPresence(at(ms(1500)), "b", true)
	entry := at(h.cfg.TCountdown)
	h.r.tick(entry)
	require.Equal(t, stateHalf1, h.r.state)
	assert.Zero(t, h.r.byID["b"].graceUsed, "the countdown drop cost nothing")

	h = twoPlayers(t)
	h.r.setPresence(at(0), "a", true)
	h.r.setPresence(at(0), "b", true)
	h.r.setPresence(at(ms(500)), "b", false)
	h.r.tick(entry)
	require.Equal(t, stateGrace, h.r.state)
	h.r.setPresence(entry.Add(time.Second), "b", true)
	require.Equal(t, stateHalf1, h.r.state)
	assert.Equal(t, time.Second, h.r.byID["b"].graceUsed, "charged from the grace entry")
}

// TestGraceDropStillCappedAtTGrace: with all 15 s of the total left, one
// drop still gets 5 s, not 15.
func TestGraceDropStillCappedAtTGrace(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	drop := start.Add(time.Second)
	h.r.setPresence(drop, "b", false)
	h.r.tick(drop.Add(h.cfg.TGrace))

	require.True(t, h.r.finished())
	assert.Equal(t, drop.Add(h.cfg.TGrace), h.r.endedAt)
}
