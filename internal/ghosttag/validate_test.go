package ghosttag

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moving is a player with a previous packet at the origin, heading theta,
// validated at t0, with a full slack budget (25).
func moving(theta float64) motion {
	return motion{pos: vec{}, theta: theta, ts: t0, hasPrev: true, slack: 25}
}

func validate(t *testing.T, m motion, r role, raw vec, after time.Duration, radius float64) (motion, bool) {
	t.Helper()
	c := DefaultConfig()
	next, clipped, err := validateMove(&c, m, r, raw, t0.Add(after), radius)
	require.NoError(t, err)
	return next, clipped
}

// TestValidateClipsSpeed: a step longer than V·Δt + slack is cut to that
// length along the same direction, and spends the slack; a shorter one
// passes untouched and saves what it did not use. At 40 ms with a full slack
// the hunter may cover 225·0.04 + 25 = 34 units and the runner
// 260·0.04 + 25 = 35.4.
func TestValidateClipsSpeed(t *testing.T) {
	next, clipped := validate(t, moving(0), roleHunter, vec{100, 0}, ms(40), 500)
	assert.True(t, clipped)
	assert.InDelta(t, 34, next.pos.X, 1e-9)
	assert.InDelta(t, 0, next.pos.Y, 1e-9)
	assert.InDelta(t, 0, next.slack, 1e-9, "the slack is spent")

	next, clipped = validate(t, moving(0), roleRunner, vec{100, 0}, ms(40), 500)
	assert.True(t, clipped)
	assert.InDelta(t, 35.4, next.pos.X, 1e-9)

	next, clipped = validate(t, moving(0), roleHunter, vec{20, 0}, ms(40), 500)
	assert.False(t, clipped)
	assert.InDelta(t, 20, next.pos.X, 1e-9)
	assert.Equal(t, t0.Add(ms(40)), next.ts)
	assert.InDelta(t, 14, next.slack, 1e-9, "25 + 9 allowed − 20 used")
	saver := moving(0)
	saver.slack = 20
	next, _ = validate(t, saver, roleHunter, vec{1, 0}, ms(40), 500)
	assert.InDelta(t, 25, next.slack, 1e-9, "20 + 9 − 1 = 28, capped at 25")
}

// TestValidateClampsTheTimeStep: a packet 2 s after the previous one gets
// Δt_max (0.25 s), not 2 s, so a backlog of packets cannot teleport; one
// 5 ms after gets its real 5 ms, not raised to Δt_min.
func TestValidateClampsTheTimeStep(t *testing.T) {
	next, _ := validate(t, moving(0), roleHunter, vec{1000, 0}, 2*time.Second, 500)
	assert.InDelta(t, 225*0.25+25, next.pos.X, 1e-9)

	next, _ = validate(t, moving(0), roleHunter, vec{1000, 0}, ms(5), 500)
	assert.InDelta(t, 225*0.005+25, next.pos.X, 1e-9)
}

// TestValidateRunnerTurnIsClippedHunterTurnIsNot: both ask to reverse
// direction in one 40 ms packet. The runner may turn only ω_max·Δt =
// 0.168 rad and goes on almost straight; the hunter turns all the way.
func TestValidateRunnerTurnIsClippedHunterTurnIsNot(t *testing.T) {
	raw := vec{-30, 0}

	runner, clipped := validate(t, moving(0), roleRunner, raw, ms(40), 500)
	assert.True(t, clipped)
	assert.InDelta(t, 4.2*0.04, runner.theta, 1e-9)
	assert.InDelta(t, 30*math.Cos(0.168), runner.pos.X, 1e-9, "the step keeps its length, along the clipped heading")
	assert.InDelta(t, 30*math.Sin(0.168), runner.pos.Y, 1e-9)

	hunter, clipped := validate(t, moving(0), roleHunter, raw, ms(40), 500)
	assert.False(t, clipped)
	assert.InDelta(t, math.Pi, math.Abs(hunter.theta), 1e-9)
	assert.InDelta(t, -30, hunter.pos.X, 1e-9)
}

// TestValidateRunnerSpeedFallsAsTheTurnSharpens: V_izin =
// V_kaçak·(1 − α·|Δθ_geçerli|/π). At Δt = 0.25 s the turn limit is
// 1.05 rad, so turns of 0, 0.5 and 1.0 rad pass whole and 2.0 rad is cut
// to 1.05; each far target is distance-clipped to V_izin·Δt + the full slack (25).
func TestValidateRunnerSpeedFallsAsTheTurnSharpens(t *testing.T) {
	stepFor := func(turn float64) float64 {
		next, clipped := validate(t, moving(0), roleRunner, polar(400, turn), ms(250), 500)
		require.True(t, clipped)
		return next.pos.norm()
	}
	want := func(applied float64) float64 {
		return 260*(1-0.45*applied/math.Pi)*0.25 + 25
	}

	s0, s1, s2, s3 := stepFor(0), stepFor(0.5), stepFor(1.0), stepFor(2.0)
	assert.InDelta(t, 90, s0, 1e-9)
	assert.InDelta(t, want(0.5), s1, 1e-9)
	assert.InDelta(t, want(1.0), s2, 1e-9)
	assert.InDelta(t, want(4.2*0.25), s3, 1e-9)
	assert.Greater(t, s0, s1)
	assert.Greater(t, s1, s2)
	assert.Greater(t, s2, s3)

	// The hunter pays no turn penalty.
	hunter, _ := validate(t, moving(0), roleHunter, polar(400, 1.0), ms(250), 500)
	assert.InDelta(t, 225*0.25+25, hunter.pos.norm(), 1e-9)
}

// TestArenaRadiusShrinks: R0 until the last 15 s of the half, then linear to
// RMin at 45 s, and no further.
func TestArenaRadiusShrinks(t *testing.T) {
	c := DefaultConfig()
	assert.Equal(t, 500.0, arenaRadius(&c, 0))
	assert.Equal(t, 500.0, arenaRadius(&c, 30*time.Second))
	assert.InDelta(t, 390, arenaRadius(&c, 37500*time.Millisecond), 1e-9)
	assert.InDelta(t, 280, arenaRadius(&c, 45*time.Second), 1e-9)
	assert.InDelta(t, 280, arenaRadius(&c, 60*time.Second), 1e-9)
}

// TestValidateScalesOntoTheArena: a position outside R(t) is scaled onto the
// circle along its own direction, also when the player stands still while
// the arena shrinks past them.
func TestValidateScalesOntoTheArena(t *testing.T) {
	m := motion{pos: vec{480, 0}, ts: t0, hasPrev: true, slack: 25}
	next, clipped := validate(t, m, roleHunter, vec{500, 20}, ms(40), 390)
	assert.True(t, clipped)
	// (500, 20) is within one step of (480, 0) and is scaled, not moved.
	k := 390 / math.Hypot(500, 20)
	assert.InDelta(t, 500*k, next.pos.X, 1e-9)
	assert.InDelta(t, 20*k, next.pos.Y, 1e-9)

	still, clipped := validate(t, m, roleRunner, vec{480, 0}, ms(40), 390)
	assert.True(t, clipped)
	assert.InDelta(t, 390, still.pos.X, 1e-9)
	assert.InDelta(t, 0, still.pos.Y, 1e-9)
}

// TestValidateFirstPacketUsesDtInitial: on the first packet after a spawn
// there is no previous ServerTs. The step is bounded by Δt_initial
// (225·0.04 + the full slack 25 = 34 units for the hunter), not Δt_max (81.25), however
// long ago the spawn was; the heading is taken from the packet without the
// turn check, and the motion starts.
func TestValidateFirstPacketUsesDtInitial(t *testing.T) {
	spawn := motion{pos: vec{-225, 0}, slack: 25}

	next, clipped := validate(t, spawn, roleHunter, vec{0, 0}, 10*time.Second, 500)
	assert.True(t, clipped)
	assert.InDelta(t, -225+34, next.pos.X, 1e-9)
	assert.True(t, next.hasPrev)
	assert.Equal(t, t0.Add(10*time.Second), next.ts)

	// A runner spawned facing the centre (θ = π) may leave straight away
	// from it: no turn rate applies to the first packet.
	runnerSpawn := motion{pos: vec{225, 0}, theta: math.Pi, slack: 25}
	next, clipped = validate(t, runnerSpawn, roleRunner, vec{255, 0}, 0, 500)
	assert.False(t, clipped)
	assert.InDelta(t, 255, next.pos.X, 1e-9)
	assert.InDelta(t, 0, next.theta, 1e-9)
}

// TestValidateRejectsNonFinite: NaN and ±Inf are refused and leave the
// motion as it was. JSON cannot carry them; the bot calls the same
// function, so the function checks.
func TestValidateRejectsNonFinite(t *testing.T) {
	c := DefaultConfig()
	m := moving(0.3)
	for _, raw := range []vec{
		{math.NaN(), 0}, {0, math.NaN()}, {math.Inf(1), 0}, {0, math.Inf(-1)}, {math.NaN(), math.Inf(1)},
		{math.MaxFloat64, math.MaxFloat64},
	} {
		next, clipped, err := validateMove(&c, m, roleRunner, raw, t0.Add(ms(40)), 500)
		assert.ErrorIs(t, err, errNonFinite, "%v", raw)
		assert.False(t, clipped)
		assert.Equal(t, m, next)
	}
}
