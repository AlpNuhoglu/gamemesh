package ghosttag

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The runner's turn limit with no floor on the time step (section 1.2,
// step 1), the micro-movement rule, and two measured prices of having no
// turn credit.

// turnAtMax sends a runner rate packets in one second from an established
// motion, each asking for a point far off at a right angle to its heading,
// so every packet asks for more turn than it is allowed. It returns the
// total turn and the path length travelled.
func turnAtMax(t *testing.T, c *Config, rate int, slack float64) (turned, travelled float64) {
	t.Helper()
	m := motion{pos: vec{}, theta: 0, ts: t0, hasPrev: true, slack: slack}
	for k := 1; k <= rate; k++ {
		raw := m.pos.add(polar(1000, m.theta+math.Pi/2))
		next, _, err := validateMove(c, m, roleRunner, raw, t0.Add(time.Duration(k)*time.Second/time.Duration(rate)), noArena)
		require.NoError(t, err)
		turned += wrapPi(next.theta - m.theta)
		travelled += next.pos.dist(m.pos)
		m = next
	}
	return turned, travelled
}

// TestRunnerTurnPerSecondIsRateIndependent is the owner's acceptance
// criterion: from an established motion, the runner's largest turn in one
// second is exactly ω_max (4.2 rad) at 10, 25, 60 and 120 packets a second.
// The real intervals of a second add up to a second; with the old 20 ms
// floor it was 5.04 rad at 60 Hz and 10.08 at 120 Hz.
func TestRunnerTurnPerSecondIsRateIndependent(t *testing.T) {
	c := DefaultConfig()
	for _, rate := range []int{10, 25, 60, 120} {
		turned, _ := turnAtMax(t, &c, rate, c.SlackMax)
		t.Logf("runner, %3d Hz: %.4f rad turned in 1 s", rate, turned)
		assert.InDelta(t, c.OmegaMax, turned, 1e-9, "%d Hz", rate)
	}
}

// TestMicroMovementDoesNotTurnTheRunner: a runner heading along +x gets a
// packet 5e-4 away at a right angle, then 10 ms later a real step straight
// ahead. The first is standing still: no heading is derived from it, so the
// real step needs no turn and is not clipped. Deriving a heading from the
// noise would have thrown θ by 0.168 rad and clipped the real step, which
// may turn only 0.042 rad back in 10 ms.
func TestMicroMovementDoesNotTurnTheRunner(t *testing.T) {
	c := DefaultConfig()
	m := motion{pos: vec{}, theta: 0, ts: t0, hasPrev: true, slack: c.SlackMax}

	m, clipped, err := validateMove(&c, m, roleRunner, vec{0, 5e-4}, t0.Add(ms(40)), 500)
	require.NoError(t, err)
	assert.False(t, clipped)
	assert.Equal(t, vec{}, m.pos)
	assert.Equal(t, 0.0, m.theta)
	assert.Equal(t, t0.Add(ms(40)), m.ts)

	m, clipped, err = validateMove(&c, m, roleRunner, vec{10, 0}, t0.Add(ms(50)), 500)
	require.NoError(t, err)
	assert.False(t, clipped, "the real step is not turn-clipped")
	assert.Equal(t, 0.0, m.theta)
	assert.InDelta(t, 10, m.pos.X, 1e-12)
}

// TestMicroMovementThreshold: a displacement just over ε_hareket is
// processed normally, heading and all; what falls under it does not
// accumulate, because each request is an absolute position measured from
// the validated one.
func TestMicroMovementThreshold(t *testing.T) {
	c := DefaultConfig()
	m := motion{pos: vec{}, theta: 0, ts: t0, hasPrev: true, slack: c.SlackMax}

	next, _, err := validateMove(&c, m, roleRunner, vec{0, 1.1e-3}, t0.Add(ms(40)), 500)
	require.NoError(t, err)
	assert.InDelta(t, 4.2*0.04, next.theta, 1e-12, "a heading is derived, turn-limited")
	assert.InDelta(t, 1.1e-3, next.pos.norm(), 1e-12)

	// 4e-4 and 8e-4 from the start are ignored; 1.2e-3 is not.
	for i, x := range []float64{4e-4, 8e-4, 1.2e-3} {
		m, _, err = validateMove(&c, m, roleHunter, vec{x, 0}, t0.Add(time.Duration(i+1)*ms(40)), 500)
		require.NoError(t, err)
	}
	assert.InDelta(t, 1.2e-3, m.pos.X, 1e-15)
}

// TestRepeatedMicroMovementsAreNotClamps: a client jittering under ε_hareket
// around its position is neither moved nor told about a correction, and no
// clamp is counted.
func TestRepeatedMicroMovementsAreNotClamps(t *testing.T) {
	h := twoPlayers(t)
	start := h.started()
	p := h.runner()
	h.walk(start.Add(ms(40)), p, vec{225, 100}, 3)
	anchor := p.mot.pos
	clamps, told := h.obs[EventClamp], len(h.out.ofType("clamp"))

	for k := 1; k <= 20; k++ {
		jitter := polar(9e-4, float64(k))
		// 500 ms apart, past the clamp message interval, so a correction
		// would be told every time.
		h.send(start.Add(time.Second+time.Duration(k)*ms(500)), p, anchor.X+jitter.X, anchor.Y+jitter.Y)
	}

	assert.Equal(t, anchor, p.mot.pos)
	assert.Equal(t, clamps, h.obs[EventClamp])
	assert.Len(t, h.out.ofType("clamp"), told)
}

// TestMeasureBunchedTurn measures, without changing anything, the price of
// having no turn credit (section 11). A client runner turns at the maximum
// rate at 25 Hz: each position is one step of V_izin·0.04 along a heading
// ω_max·0.04 further round. Its packets 1-3 arrive as an 80 ms gap and then
// three 1 ms apart, then packets 4-6 arrive on time. The values asserted are
// the ones measured, so a change to the rule shows here.
func TestMeasureBunchedTurn(t *testing.T) {
	c := DefaultConfig()
	turn := c.OmegaMax * 0.04
	step := c.VRunner * (1 - c.Alpha*turn/math.Pi) * 0.04
	client := []vec{{}}
	heading := 0.0
	for k := 1; k <= 6; k++ {
		heading += turn
		client = append(client, client[k-1].add(polar(step, heading)))
	}
	arrive := []time.Duration{0, ms(80), ms(81), ms(82), ms(160), ms(200), ms(240)}

	m := motion{pos: vec{}, theta: 0, ts: t0, hasPrev: true, slack: c.SlackMax}
	var off []float64
	clips := 0
	for k := 1; k <= 6; k++ {
		next, clipped, err := validateMove(&c, m, roleRunner, client[k], t0.Add(arrive[k]), noArena)
		require.NoError(t, err)
		if clipped {
			clips++
		}
		m = next
		off = append(off, m.pos.dist(client[k]))
		t.Logf("packet %d at %3d ms: clipped %-5v, %.4f units from the request, heading %.4f (client %.4f)",
			k, arrive[k]/time.Millisecond, clipped, off[k-1], m.theta, float64(k)*turn)
	}
	assert.Equal(t, 0.0, off[0], "the first, after the gap, is not clipped")
	assert.Positive(t, off[2], "the third of the bunch is clipped")
	t.Logf("clipped packets: %d of 6; off by %.4f after the bunch, %.4f after three on-time packets", clips, off[2], off[5])
}

// TestMeasureTurnPenaltyByRate measures, without changing anything, how far
// a runner turning at the maximum rate travels in one second at each
// packet rate, with no slack to blur it. V_izin uses the turn of one step,
// so the penalty depends on the rate: 260·(1 − α·ω_max/(π·rate)). A finding
// for the owner; the values asserted are that formula, which the
// measurement matches.
func TestMeasureTurnPenaltyByRate(t *testing.T) {
	c := DefaultConfig()
	for _, rate := range []int{10, 25, 60, 120} {
		_, travelled := turnAtMax(t, &c, rate, 0)
		want := c.VRunner * (1 - c.Alpha*c.OmegaMax/(math.Pi*float64(rate)))
		t.Logf("runner turning at the maximum, %3d Hz: %.2f units in 1 s", rate, travelled)
		assert.InDelta(t, want, travelled, 1e-9, "%d Hz", rate)
	}
}
