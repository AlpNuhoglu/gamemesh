package ghosttag

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The DISCONNECT_GRACE deadline is per player (section 2, "Sayaç oyuncu
// başınadır"). All on a fake clock.

// TestGraceDeadlineIsPerPlayer is the case a room-wide deadline got wrong:
// A drops at t, B at t + 4.9 s, A is back at t + 4.95 s. B has their own
// 5 s and forfeits at t + 9.9 s, not at t + 5 s after 0.1 s away.
func TestGraceDeadlineIsPerPlayer(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	drop := start.Add(time.Second)
	h.r.setPresence(drop, "a", false)
	h.r.setPresence(drop.Add(4900*time.Millisecond), "b", false)
	h.r.setPresence(drop.Add(4950*time.Millisecond), "a", true)

	h.r.tick(drop.Add(h.cfg.TGrace))
	require.Equal(t, stateGrace, h.r.state, "B's grace is not A's")
	h.r.tick(drop.Add(9900*time.Millisecond - time.Millisecond))
	require.Equal(t, stateGrace, h.r.state)

	h.r.tick(drop.Add(9900 * time.Millisecond))
	require.True(t, h.r.finished())
	assert.Equal(t, outcomeForfeit, h.r.result.outcome)
	assert.Equal(t, "b", h.r.result.leaver)
	assert.Equal(t, drop.Add(9900*time.Millisecond), h.r.endedAt)
}

// TestExpiredPlayerForfeitsWhenTheOpponentReturns: A's deadline passes while
// B is also gone, but B is still within theirs. The room waits for B; A
// coming back meanwhile changes nothing; when B returns in time the match
// ends then, with A as the leaver.
func TestExpiredPlayerForfeitsWhenTheOpponentReturns(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	drop := start.Add(time.Second)
	h.r.setPresence(drop, "a", false)
	h.r.setPresence(drop.Add(time.Second), "b", false)

	h.r.tick(drop.Add(h.cfg.TGrace))
	require.Equal(t, stateGrace, h.r.state)
	require.True(t, h.r.byID["a"].expired)

	h.r.setPresence(drop.Add(h.cfg.TGrace+ms(100)), "a", true)
	require.Equal(t, stateGrace, h.r.state, "an expired player's return does not resume the room")
	assert.True(t, h.r.byID["a"].expired, "nor undo the forfeit")

	back := drop.Add(h.cfg.TGrace + ms(500))
	h.r.setPresence(back, "b", true)
	require.True(t, h.r.finished())
	assert.Equal(t, outcomeForfeit, h.r.result.outcome)
	assert.Equal(t, "a", h.r.result.leaver)
	assert.Equal(t, "b", decide(&h.cfg, h.r.result).winner)
	assert.Equal(t, back, h.r.endedAt)
	assert.Equal(t, 1, h.obs[EventForfeit])
	assert.Zero(t, h.obs[EventBothDisconnected])
}

// TestGraceWaitsForEveryone: A comes back in time while B is still gone;
// the room stays in DISCONNECT_GRACE with the half clock stopped, and
// resumes only when B is back too, with the clock where it stopped.
func TestGraceWaitsForEveryone(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	drop := start.Add(10 * time.Second)
	h.r.setPresence(drop, "a", false)
	h.r.setPresence(drop.Add(time.Second), "b", false)

	h.r.setPresence(drop.Add(2*time.Second), "a", true)
	require.Equal(t, stateGrace, h.r.state)
	assert.True(t, h.r.byID["a"].deadline.IsZero(), "A's deadline is cleared")
	assert.Equal(t, 10*time.Second, h.r.elapsed(drop.Add(2*time.Second)))

	back := drop.Add(5 * time.Second)
	h.r.setPresence(back, "b", true)
	require.Equal(t, stateHalf1, h.r.state)
	assert.Equal(t, 10*time.Second, h.r.elapsed(back))
	assert.Equal(t, 10*time.Second+ms(40), h.r.elapsed(back.Add(ms(40))))
}

// TestGoneAtCountdownEndGetsTheWholeGrace: a player who dropped during the
// countdown, and is gone when it ends, has TGrace from the room's entry
// into DISCONNECT_GRACE, not from the drop.
func TestGoneAtCountdownEndGetsTheWholeGrace(t *testing.T) {
	h := twoPlayers(t)
	h.r.setPresence(at(0), "a", true)
	h.r.setPresence(at(0), "b", true)
	h.r.setPresence(at(ms(500)), "b", false)
	assert.True(t, h.r.byID["b"].deadline.IsZero(), "a drop in the countdown sets no deadline by itself")

	entry := at(h.cfg.TCountdown)
	h.r.tick(entry)
	require.Equal(t, stateGrace, h.r.state)
	h.r.tick(entry.Add(h.cfg.TGrace - time.Millisecond))
	require.Equal(t, stateGrace, h.r.state)

	h.r.tick(entry.Add(h.cfg.TGrace))
	require.True(t, h.r.finished())
	assert.Equal(t, "b", h.r.result.leaver)
	assert.Equal(t, entry.Add(h.cfg.TGrace), h.r.endedAt)
}

// TestBotMatchHumanExpiresForfeits: the bot never leaves; a human whose
// deadline passes forfeits to it.
func TestBotMatchHumanExpiresForfeits(t *testing.T) {
	h, start := botMatch(t, DefaultConfig())
	drop := start.Add(time.Second)
	h.r.setPresence(drop, "human", false)
	require.Equal(t, stateGrace, h.r.state)

	h.r.tick(drop.Add(h.cfg.TGrace))

	require.True(t, h.r.finished())
	assert.Equal(t, outcomeForfeit, h.r.result.outcome)
	assert.Equal(t, "human", h.r.result.leaver)
	assert.Equal(t, "bot", decide(&h.cfg, h.r.result).winner)
	assert.Zero(t, h.obs[EventBothDisconnected])
}
