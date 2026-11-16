package ghosttag

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A client that always asks for a point far ahead on a straight line, as a
// speed cheat would. The arena is made huge so that only the distance
// budget limits it.
const noArena = 1e12

// greedy sends packets at rate Hz from m, the k-th at t0 + k/rate, k = 1..n,
// each asking for a point 1000 units further along +x, and returns the
// motion after each packet.
func greedy(t *testing.T, c *Config, m motion, r role, rate, n int) []motion {
	t.Helper()
	out := make([]motion, 0, n)
	for k := 1; k <= n; k++ {
		now := t0.Add(time.Duration(k) * time.Second / time.Duration(rate))
		next, _, err := validateMove(c, m, r, m.pos.add(vec{1000, 0}), now, noArena)
		require.NoError(t, err)
		m = next
		out = append(out, m)
	}
	return out
}

// TestSlackDistanceIsRateIndependent is the owner's acceptance criterion
// for the slack budget (section 1.2, step 3). From an established motion
// (a previous packet at t0) with a full slack, a greedy client covers in
// the first second exactly V + S_max, and in the next second exactly V,
// whether it sends 10, 25, 60 or 120 packets a second. The limit is on the
// distance, not per packet: every row is at most nominal + 25, the rows do
// not depend on the rate, and the runner stays as much faster than the
// hunter as the nominal speeds say.
func TestSlackDistanceIsRateIndependent(t *testing.T) {
	c := DefaultConfig()
	for _, tc := range []struct {
		name string
		r    role
		v    float64
	}{{"hunter", roleHunter, c.VHunter}, {"runner", roleRunner, c.VRunner}} {
		for _, rate := range []int{10, 25, 60, 120} {
			start := motion{pos: vec{}, theta: 0, ts: t0, hasPrev: true, slack: c.SlackMax}
			steps := greedy(t, &c, start, tc.r, rate, 2*rate)
			first := steps[rate-1].pos.X
			second := steps[2*rate-1].pos.X - first
			t.Logf("%s %3d Hz: first second %.4f, next second %.4f", tc.name, rate, first, second)
			assert.InDelta(t, tc.v+c.SlackMax, first, 1e-6, "%s at %d Hz, first second", tc.name, rate)
			assert.InDelta(t, tc.v, second, 1e-6, "%s at %d Hz, next second", tc.name, rate)
		}
	}
}

// TestSlackFromAFreshSpawn prints, in the form of the document's "before"
// table, how far a greedy client gets in the first second after a spawn:
// the first packet one interval after the spawn, measured with DtInitial,
// then one every interval up to 1 s. It asserts only the bound that
// follows from the rule: at most V·(1 − interval + DtInitial) + S_max.
func TestSlackFromAFreshSpawn(t *testing.T) {
	c := DefaultConfig()
	for _, tc := range []struct {
		name string
		r    role
		v    float64
	}{{"hunter", roleHunter, c.VHunter}, {"runner", roleRunner, c.VRunner}} {
		for _, rate := range []int{25, 60} {
			spawn := motion{pos: vec{}, theta: 0, slack: c.SlackMax}
			steps := greedy(t, &c, spawn, tc.r, rate, rate)
			got := steps[rate-1].pos.X
			interval := 1 / float64(rate)
			want := tc.v*(1-interval+c.DtInitial.Seconds()) + c.SlackMax
			t.Logf("| %s, %d Hz | %.1f | %.0f |", tc.name, rate, got, tc.v)
			assert.InDelta(t, want, got, 1e-6, "%s at %d Hz", tc.name, rate)
		}
	}
}

// TestSlackIsFullAtSpawnAndAtTheSecondHalf: a half starts with S_max for
// both players, whatever they spent in the half before.
func TestSlackIsFullAtSpawnAndAtTheSecondHalf(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	for _, p := range h.r.players {
		assert.Equal(t, h.cfg.SlackMax, p.mot.slack, p.id)
	}
	h.walk(start.Add(ms(40)), h.hunter(), vec{0, 400}, 5)
	require.Zero(t, h.hunter().mot.slack)

	h.r.tick(start.Add(h.cfg.THalf + h.cfg.TRoleSwap))
	require.Equal(t, stateHalf2, h.r.state)
	for _, p := range h.r.players {
		assert.Equal(t, h.cfg.SlackMax, p.mot.slack, p.id)
	}
}

// TestSlackFillsWhileStandingStillUpToTheCap: a player who stops accrues
// the distance they did not use, at their base speed, up to S_max and no
// further; a first packet that does not move accrues nothing.
func TestSlackFillsWhileStandingStillUpToTheCap(t *testing.T) {
	c := DefaultConfig()
	m := motion{pos: vec{}, ts: t0, hasPrev: true}
	for k := 1; k <= 50; k++ { // 2 s standing still at 25 Hz
		next, clipped, err := validateMove(&c, m, roleHunter, m.pos, t0.Add(time.Duration(k)*ms(40)), 500)
		require.NoError(t, err)
		require.False(t, clipped)
		m = next
		if k == 1 {
			assert.InDelta(t, 225*0.04, m.slack, 1e-9, "one step's worth")
		}
	}
	assert.Equal(t, c.SlackMax, m.slack, "capped")

	next, _, err := validateMove(&c, m, roleHunter, vec{1000, 0}, m.ts.Add(ms(40)), 500)
	require.NoError(t, err)
	assert.InDelta(t, 225*0.04+25, next.pos.X, 1e-9, "the step after spends the whole cap and no more")

	fresh := motion{pos: vec{-225, 0}, slack: 3}
	next, _, err = validateMove(&c, fresh, roleHunter, fresh.pos, t0, 500)
	require.NoError(t, err)
	assert.Equal(t, 3.0, next.slack, "a first packet that does not move accrues nothing")
}

// TestSlackIsChargedWithTheStepBeforeTheArena: a hunter 10 units from the
// edge asks for a point far outside it. The validated step is 34 (9 of
// budget and all 25 of slack); the arena then scales the position back to
// the edge, a displacement of only 10. The slack is charged with the step,
// so it is empty, not 24: pressing into the wall is not a way to save it.
func TestSlackIsChargedWithTheStepBeforeTheArena(t *testing.T) {
	c := DefaultConfig()
	m := motion{pos: vec{490, 0}, ts: t0, hasPrev: true, slack: c.SlackMax}

	next, clipped, err := validateMove(&c, m, roleHunter, vec{600, 0}, t0.Add(ms(40)), 500)

	require.NoError(t, err)
	assert.True(t, clipped)
	assert.InDelta(t, 500, next.pos.X, 1e-9)
	assert.InDelta(t, 0, next.slack, 1e-9)
}

// TestBunchedPacketsAreNotClipped: a legitimate hunter at top speed sends
// every 40 ms, but after its packet at t0 the next three arrive bunched: an
// 80 ms gap, then three packets 1 ms apart. Measured on the server, they
// claim 27 units in 82 ms. The first packet has the 80 ms gap's budget
// (18) plus the slack the mover kept by never exceeding V; it uses 9 and the
// slack stays at the cap (25 + 18 − 9 is capped at 25). The next two have
// 0.225 of budget each and spend 8.775 of slack each. It ends where it asked
// to be, unclipped, with 7.45 of slack left.
func TestBunchedPacketsAreNotClipped(t *testing.T) {
	c := DefaultConfig()
	m := motion{pos: vec{}, ts: t0, hasPrev: true, slack: c.SlackMax}
	for i, at := range []time.Duration{ms(80), ms(81), ms(82)} {
		want := vec{225 * 0.04 * float64(i+1), 0}
		next, clipped, err := validateMove(&c, m, roleHunter, want, t0.Add(at), 500)
		require.NoError(t, err)
		assert.False(t, clipped, "packet %d", i+1)
		m = next
	}
	assert.InDelta(t, 27, m.pos.X, 1e-9)
	assert.InDelta(t, 25-2*(9-225*0.001), m.slack, 1e-9)
}

// TestRunnerTurnPerSecondByRate measures, and does not fix, the runner's
// largest total turn in one second at different packet rates. The turn
// limit still uses Δt_θ = max(Δt, Δt_min), so above 50 Hz every packet is
// allowed ω_max·0.02 however close it follows the last one. Reported to the
// owner as a finding; only the 25 Hz value (ω_max·1 s) is asserted.
func TestRunnerTurnPerSecondByRate(t *testing.T) {
	c := DefaultConfig()
	turned := map[int]float64{}
	for _, rate := range []int{25, 60, 120} {
		m := motion{pos: vec{}, theta: 0, ts: t0, hasPrev: true, slack: c.SlackMax}
		total := 0.0
		for k := 1; k <= rate; k++ {
			// Ask for a point at a right angle to the heading: always more
			// than one packet's turn allows.
			raw := m.pos.add(polar(10, m.theta+math.Pi/2))
			next, _, err := validateMove(&c, m, roleRunner, raw, t0.Add(time.Duration(k)*time.Second/time.Duration(rate)), noArena)
			require.NoError(t, err)
			total += wrapPi(next.theta - m.theta)
			m = next
		}
		turned[rate] = total
		t.Logf("runner, %3d Hz: %.4f rad turned in 1 s", rate, total)
	}
	assert.InDelta(t, c.OmegaMax, turned[25], 1e-9)
}
