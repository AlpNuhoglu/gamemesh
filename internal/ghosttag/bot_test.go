package ghosttag

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// botMatch is a started bot match: the bot hunts first, so it is the
// hunter in HALF_1 from the returned time.
func botMatch(t *testing.T, cfg Config) (*harness, time.Time) {
	h := newHarness(t, cfg, "bot-room", []string{"human"}, "bot")
	h.r.setPresence(at(0), "human", true)
	start := at(cfg.TCountdown)
	h.r.tick(start)
	require.Equal(t, stateHalf1, h.r.state)
	return h, start
}

// TestBotCannotExceedThePhysics: a bot brain that believes it is ten times
// faster than it is still moves no further than the pipeline allows a
// human: over five ticks V·5·Tick plus the slack it had (full, since the
// half's first step was at nominal speed), and on any one tick at most
// V·Tick + slack. Each excess is counted as a bot clamp, apart from the
// human clamp count.
func TestBotCannotExceedThePhysics(t *testing.T) {
	h, start := botMatch(t, DefaultConfig())
	fast := DefaultConfig()
	fast.VHunter, fast.VRunner = 10*fast.VHunter, 10*fast.VRunner
	h.r.botHunter = botHunter{cfg: &fast}
	bot := h.r.bot
	before := bot.hist.len() // the spawn and the step of the tick that began the half

	require.InDelta(t, 25, bot.mot.slack, 1e-9)

	from, prev := bot.mot.pos, bot.mot.pos
	for i := 1; i <= 5; i++ {
		h.r.tick(start.Add(time.Duration(i) * h.cfg.Tick))
		step := bot.mot.pos.dist(prev)
		assert.LessOrEqual(t, step, 225*0.04+25+1e-9, "tick %d", i)
		assert.GreaterOrEqual(t, step, 225*0.04-1e-9, "tick %d: the bot still moves", i)
		prev = bot.mot.pos
	}
	assert.InDelta(t, 225*5*0.04+25, bot.mot.pos.dist(from), 1e-9)
	assert.Equal(t, before+5, bot.hist.len(), "every step is in the bot's ring")
	assert.Equal(t, 5, h.obs[EventBotClamp])
	assert.Zero(t, h.obs[EventClamp])
	assert.Len(t, h.out.ofType("clamp"), 0, "nobody is told about the bot's corrections")
}

// TestBotStepsAtTheTickRate: the bot moves once per tick, from the tick that
// begins the half, and each step is broadcast as a pos from the bot.
func TestBotStepsAtTheTickRate(t *testing.T) {
	h, start := botMatch(t, DefaultConfig())
	for i := 1; i <= 25; i++ {
		h.r.tick(start.Add(time.Duration(i) * h.cfg.Tick))
	}
	pos := h.out.ofType("pos")
	require.Len(t, pos, 26)
	for _, m := range pos {
		assert.Equal(t, "bot", m.body["from"])
		assert.Equal(t, "except", m.how)
	}
	assert.Zero(t, h.obs[EventBotClamp], "the bot's own steps stay within the physics")
}

// hunterTrail is a hunter ring with one sample every 40 ms over [0, 4 s]
// at t0: at x = -200 until cut, then wherever late says.
func hunterTrail(cut time.Duration, late vec) *ring {
	r := newRing(160)
	for d := time.Duration(0); d <= 4*time.Second; d += ms(40) {
		p := vec{-200, 0}
		if d > cut {
			p = late
		}
		r.push(sample{ts: t0.Add(d), p: p})
	}
	return &r
}

// runnerSteps runs a bot runner for ten ticks from now = t0 + 4 s, with
// the hunter ring given, and returns its targets.
func runnerSteps(cfg *Config, hunter *ring) []vec {
	b := botRunner{cfg: cfg, rng: rand.New(rand.NewPCG(9, 9))}
	self := motion{pos: vec{100, 50}, theta: 0, ts: t0.Add(4*time.Second - cfg.Tick), hasPrev: true}
	var out []vec
	for i := 0; i < 10; i++ {
		now := t0.Add(4*time.Second + time.Duration(i)*cfg.Tick)
		target := b.next(self, now, 500, delayedSight(hunter, now, cfg.BotSightDelay, vec{-225, 0}))
		out = append(out, target)
		self = motion{pos: target, theta: self.theta, ts: now, hasPrev: true}
	}
	return out
}

// TestBotRunnerSeesOnlyTheDelayedHunter: two hunters with the same trail up
// to 2000 ms before the bot's last step, one of which then jumps next to
// the bot. The bot runner moves identically in both: it cannot see
// anything the hunter did in the last 2000 ms. A hunter that differs
// earlier does change its moves, so the comparison is not vacuous.
func TestBotRunnerSeesOnlyTheDelayedHunter(t *testing.T) {
	c := DefaultConfig()
	lastNow := 4*time.Second + 9*c.Tick
	horizon := lastNow - c.BotSightDelay

	calm := runnerSteps(&c, hunterTrail(4*time.Second, vec{}))
	pounce := runnerSteps(&c, hunterTrail(horizon, vec{110, 50}))
	assert.Equal(t, calm, pounce, "the last 2000 ms of the hunter are invisible")

	earlier := runnerSteps(&c, hunterTrail(horizon-c.BotReactionMax-ms(400), vec{400, 50}))
	assert.NotEqual(t, calm, earlier)
}

// TestBotRunnerReactionDelay: every step asks for the hunter 180 to 260 ms
// further back than the sight delay, and the asks vary.
func TestBotRunnerReactionDelay(t *testing.T) {
	c := DefaultConfig()
	b := botRunner{cfg: &c, rng: rand.New(rand.NewPCG(3, 4))}
	seen := map[time.Duration]bool{}
	self := motion{pos: vec{100, 0}, ts: t0, hasPrev: true}
	for i := 0; i < 200; i++ {
		b.next(self, t0.Add(c.Tick), 500, func(extra time.Duration) vec {
			assert.GreaterOrEqual(t, extra, c.BotReactionMin)
			assert.LessOrEqual(t, extra, c.BotReactionMax)
			seen[extra] = true
			return vec{-100, 0}
		})
	}
	assert.Greater(t, len(seen), 50)
}

// TestDelayedSightCannotLookCloser: asking for a negative extra delay does
// not get the bot anything newer than now − BotSightDelay.
func TestDelayedSightCannotLookCloser(t *testing.T) {
	c := DefaultConfig()
	hunter := hunterTrail(2*time.Second, vec{300, 300})
	see := delayedSight(hunter, t0.Add(4*time.Second), c.BotSightDelay, vec{-225, 0})

	assert.Equal(t, vec{-200, 0}, see(-time.Second))
	assert.Equal(t, vec{-200, 0}, see(0))
	early := delayedSight(hunter, t0.Add(time.Second), c.BotSightDelay, vec{-225, 0})
	assert.Equal(t, vec{-225, 0}, early(0), "before the trail begins the hunter is at its spawn")
}

// TestBotRunnerFleesAndAvoidsTheWall: away from the hunter it last saw,
// and along the wall rather than into it.
func TestBotRunnerFleesAndAvoidsTheWall(t *testing.T) {
	c := DefaultConfig()
	b := botRunner{cfg: &c, rng: rand.New(rand.NewPCG(1, 1))}
	see := func(time.Duration) vec { return vec{-100, 0} }

	open := b.next(motion{pos: vec{0, 0}, hasPrev: false}, t0, 500, see)
	assert.Greater(t, open.X, 0.0, "flees from the hunter")

	// Running along the wall with the hunter behind it on the far side, it
	// bends inwards instead of being pushed out onto the edge.
	self := motion{pos: vec{495, 0}, theta: math.Pi / 2, ts: t0, hasPrev: true}
	edge := b.next(self, t0.Add(c.Tick), 500, see)
	assert.Less(t, edge.norm(), self.pos.norm())
}

// TestBotRunnerTurnsBeforeTheWall: a bot runner heading straight at the
// wall, fleeing a hunter behind it, starts turning botTurnRadius from the
// edge and runs along it without ever having its step cut short: every
// step is the full one the pipeline allows for its turn, and every target
// is inside the arena. Turning only at the wall, the step has to be
// shortened (insideStep) and the bot stalls against the edge.
func TestBotRunnerTurnsBeforeTheWall(t *testing.T) {
	c := DefaultConfig()
	assert.InDelta(t, 260/4.2+15, botTurnRadius(&c), 1e-9)
	b := botRunner{cfg: &c, rng: rand.New(rand.NewPCG(5, 5))}
	see := func(time.Duration) vec { return vec{-300, 0} } // behind it, on the far side

	self := motion{pos: vec{300, 0}, theta: 0, ts: t0, hasPrev: true} // heading straight out
	for i := 1; i <= 100; i++ {
		now := t0.Add(time.Duration(i) * c.Tick)
		target := b.next(self, now, 500, see)
		next, clipped, err := validateMove(&c, self, roleRunner, target, now, 500)
		require.NoError(t, err)
		require.False(t, clipped, "step %d: the pipeline corrected the bot", i)
		turn := math.Abs(wrapPi(next.theta - self.theta))
		full := c.VRunner * (1 - c.Alpha*turn/math.Pi) * c.Tick.Seconds()
		require.InDelta(t, full, next.pos.dist(self.pos), 1e-9, "step %d at %.1f from the centre: cut short", i, self.pos.norm())
		self = next
	}
	assert.Greater(t, math.Abs(self.pos.Y), 100.0, "it went along the wall, not into it")
}

// TestInsideStep: the last resort keeps a step inside the arena, and only
// shortens it when it would leave.
func TestInsideStep(t *testing.T) {
	assert.Equal(t, 10.0, insideStep(vec{0, 0}, 0, 10, 500))
	s := insideStep(vec{495, 0}, 0, 10, 500)
	assert.InDelta(t, 5, s, 1e-6)
	assert.LessOrEqual(t, vec{495 + s, 0}.norm(), 500.0)
	assert.Equal(t, 0.0, insideStep(vec{500, 0}, 0, 10, 500))
}

// TestBotHunterLeadsTheRunner: the bot hunter aims ahead of a moving
// runner, from the centre side while far.
func TestBotHunterLeadsTheRunner(t *testing.T) {
	c := DefaultConfig()
	b := botHunter{cfg: &c}
	self := motion{pos: vec{-200, 0}}

	still := b.next(self, t0, 500, vec{200, 0}, vec{})
	moving := b.next(self, t0, 500, vec{200, 0}, vec{0, 200})
	assert.Greater(t, moving.Y, still.Y, "leads a runner moving up")
	assert.InDelta(t, 225*0.04, still.dist(self.pos), 1e-9, "one tick at top speed")

	near := b.next(self, t0, 500, vec{-195, 0}, vec{})
	assert.Equal(t, vec{-195, 0}, near, "within one step it goes straight at the runner")
}

// TestLiveView: the bot hunter's view of the runner is the newest sample
// and the velocity between the last two.
func TestLiveView(t *testing.T) {
	r := ringOf(4, pt{0, 0, 0}, pt{40, 4, 8})
	pos, vel := liveView(r)
	assert.Equal(t, vec{4, 8}, pos)
	assert.InDelta(t, 100, vel.X, 1e-9)
	assert.InDelta(t, 200, vel.Y, 1e-9)
}

// TestBotMatchHasNoClamp is the owner's acceptance test for the bot: a whole
// bot match, both halves to the end, and the pipeline never corrects the
// bot (bot_clamp == 0). The bot hunts in the first half and runs in the
// second, through the shrinking arena of both. It is played with steady
// ticks and with uneven ones, and against a human hunter who stands still
// (so the bot runner plays out the whole half, shrink included) and one
// who chases the bot's position at top speed.
func TestBotMatchHasNoClamp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gaps  []time.Duration
		chase bool // in the second half the human hunter chases the bot
		flee  bool // in the first half the human runner circles along the wall
	}{
		{"steady ticks, still human", []time.Duration{40 * time.Millisecond}, false, false},
		{"uneven ticks, still human", []time.Duration{25 * time.Millisecond, 55 * time.Millisecond}, false, false},
		{"very uneven ticks, still human", []time.Duration{10 * time.Millisecond, 70 * time.Millisecond}, false, false},
		{"steady ticks, chasing hunter", []time.Duration{40 * time.Millisecond}, true, false},
		{"uneven ticks, chasing hunter", []time.Duration{25 * time.Millisecond, 55 * time.Millisecond}, true, false},
		{"steady ticks, runner along the wall", []time.Duration{40 * time.Millisecond}, false, true},
		{"uneven ticks, runner along the wall", []time.Duration{25 * time.Millisecond, 55 * time.Millisecond}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, now := botMatch(t, DefaultConfig())
			human := h.r.byID["human"]
			var runnerSteps int
			for i := 0; !h.r.finished(); i++ {
				now = now.Add(tc.gaps[i%len(tc.gaps)])
				if tc.chase && h.r.state == stateHalf2 {
					h.send(now, human, h.r.bot.mot.pos.X, h.r.bot.mot.pos.Y)
				}
				if tc.flee && h.r.state == stateHalf1 {
					// A point a little ahead along the wall, just inside the
					// arena as it shrinks: the bot hunter has to follow it to
					// the edge.
					p := human.mot.pos
					a := math.Atan2(p.Y, p.X) + 0.06
					edge := h.r.radius(now) - 5
					h.send(now, human, edge*math.Cos(a), edge*math.Sin(a))
				}
				if h.r.state == stateHalf2 {
					runnerSteps++
				}
				h.r.tick(now)
				require.Less(t, i, 10000, "the match did not end")
			}
			ends := h.out.ofType("half_end")
			require.Len(t, ends, 2, "both halves were played")
			assert.Zero(t, h.obs[EventBotClamp], "the pipeline never corrected the bot")
			if !tc.chase {
				assert.Equal(t, "TIMEOUT", ends[1].body["reason"], "the bot ran out the whole second half")
				assert.Greater(t, runnerSteps, 500, "with the arena shrinking in its last 15 s")
			}
			t.Logf("first half: %s after %v ms; second half: %s after %v ms, %d bot runner ticks",
				ends[0].body["reason"], ends[0].body["dur_ms"], ends[1].body["reason"], ends[1].body["dur_ms"], runnerSteps)
		})
	}
}
