package ghosttag

import (
	"math"
	"math/rand/v2"
	"time"
)

// The bot of section 1.5. It produces one target position per tick (25 Hz,
// a human's packet rate) and the room sends that target through the same
// validateMove as a human packet, into the bot's own ring: the bot cannot
// break the physics, and every bot match exercises the pipeline.
//
// The two brains are given different inputs on purpose, and only through
// their arguments:
//
//   - botRunner (the human hunts) sees the hunter only through a sight,
//     which reads the hunter's ring no later than BotSightDelay before now:
//     the same information a human runner has, a trail 2 s behind. It adds
//     a human reaction delay on top and avoids the wall.
//   - botHunter (the human runs) sees the runner live, as a human hunter
//     does: the runner's latest validated position and the velocity derived
//     from its last two.
//
// Neither brain holds a reference to the room, a player or a ring.
//
// Both plan each step with the time step the pipeline will use for it
// (botDt), the real time since the bot's last validated step, so a tick
// that comes early or late changes how far the bot may go and turn exactly
// as it changes the pipeline's limits, and the pipeline has nothing to
// correct in ordinary play.

// botDt is the time step validateMove will use for self's next step at now:
// the real time since the last validated step, capped at DtMax, or
// DtInitial for a first step.
func botDt(c *Config, self motion, now time.Time) float64 {
	if !self.hasPrev {
		return c.DtInitial.Seconds()
	}
	return math.Max(0, math.Min(c.DtMax.Seconds(), now.Sub(self.ts).Seconds()))
}

// botTurnRadius is how far from the arena's edge the bot runner starts
// turning away from it: the radius of the tightest circle a runner can run
// at top speed (VRunner/OmegaMax, about 62 units), plus BotTurnBuffer for
// the arena that shrinks meanwhile. A runner that starts turning there can
// face along the wall before it reaches it.
func botTurnRadius(c *Config) float64 { return c.VRunner/c.OmegaMax + c.BotTurnBuffer }

// botWallInward is how much inwards the bot runner bends while it turns
// along the wall: the target direction is the tangent plus this much of the
// inward normal, about 27 degrees off the wall.
const botWallInward = 0.5

// sight returns where the hunter was BotSightDelay+extra before now. A
// negative extra counts as zero, so a brain cannot look closer to now.
type sight func(extra time.Duration) vec

// delayedSight builds the bot runner's view of the hunter from the
// hunter's ring at now. It is the only way the bot runner learns anything
// about the hunter. Before the ring reaches back far enough (the first
// seconds of a half) the hunter is where it spawned, which the snap told
// everyone.
func delayedSight(hunter *ring, now time.Time, delay time.Duration, spawn vec) sight {
	horizon := now.Add(-delay)
	return func(extra time.Duration) vec {
		if s, ok := hunter.atOrBefore(horizon.Add(-max(0, extra))); ok {
			return s.p
		}
		return spawn
	}
}

// botRunner flees what it last saw of the hunter.
type botRunner struct {
	cfg *Config
	rng *rand.Rand
}

// next returns the bot runner's target for its step at now, from self, in
// an arena of the given radius (R at now). A fresh reaction delay in
// [BotReactionMin, BotReactionMax] is drawn for every step.
func (b botRunner) next(self motion, now time.Time, radius float64, see sight) vec {
	c := b.cfg
	span := int64(c.BotReactionMax - c.BotReactionMin)
	reaction := c.BotReactionMin + time.Duration(b.rng.Int64N(span+1)) //nolint:gosec // G404: game behaviour, not a secret
	away := self.pos.sub(see(reaction)).unit()
	if away == (vec{}) {
		away = self.pos.unit()
		if away == (vec{}) {
			away = vec{1, 0}
		}
	}
	want := b.wall(self, radius, away)

	// Turn no faster than a runner may in this step, and pay the turn
	// penalty, so the pipeline has nothing to correct.
	dt := botDt(c, self, now)
	theta, turn := math.Atan2(want.Y, want.X), 0.0
	if self.hasPrev {
		limit := c.OmegaMax * dt
		turn = math.Max(-limit, math.Min(limit, wrapPi(theta-self.theta)))
		theta = wrapPi(self.theta + turn)
	}
	step := c.VRunner * (1 - c.Alpha*math.Abs(turn)/math.Pi) * dt
	return self.pos.add(polar(insideStep(self.pos, theta, step, radius), theta))
}

// wall returns the direction the bot runner wants instead of away when it
// is near the edge. Within botTurnRadius of R, heading out or wanting to,
// it runs along the wall, bent a little inwards. Which way along the wall:
// the way it is already going, so it never has to reverse at the edge (a
// reversal swings a runner out over it); only when it comes at the wall
// head on does the hunter it last saw decide, and it goes the way that
// leads away. In the band one turn radius further in, the outward part of
// its escape is taken away gradually, so it reaches that line already
// running along the wall rather than zigzagging across it.
func (b botRunner) wall(self motion, radius float64, away vec) vec {
	d := self.pos.norm()
	tr := botTurnRadius(b.cfg)
	if d == 0 || d <= radius-2*tr {
		return away
	}
	out := self.pos.scale(1 / d)
	if d <= radius-tr {
		if o := away.dot(out); o > 0 {
			w := (d - (radius - 2*tr)) / tr // 0 at the band's inner edge, 1 at its outer
			if slid := away.sub(out.scale(o * w)).unit(); slid != (vec{}) {
				return slid
			}
		}
		return away
	}
	heading := away
	if self.hasPrev {
		heading = polar(1, self.theta)
	}
	if away.dot(out) <= 0 && heading.dot(out) <= 0 {
		return away // already heading in, and wanting to
	}
	tangent := vec{-out.Y, out.X}
	side := heading.dot(tangent)
	if math.Abs(side) < botWallHeadOn {
		// Head on: the hunter's side decides, with the bit of turn already
		// begun as the tie-break, so that the choice holds from one step to
		// the next instead of flipping on a rounding error.
		side += away.dot(tangent)
	}
	if side < 0 {
		tangent = tangent.scale(-1)
	}
	return tangent.sub(out.scale(botWallInward)).unit()
}

// botWallHeadOn: a heading whose component along the wall is under this
// counts as coming at the wall head on (within about 11 degrees).
const botWallHeadOn = 0.2

// insideStep shortens step, from p in direction theta, so that it ends
// inside the arena: moving less than allowed is always legal. It is the
// last resort for a turn that came too late (an arena that shrank faster
// than the bot turned); in ordinary play the wall rule keeps the bot clear.
func insideStep(p vec, theta, step, radius float64) float64 {
	u := polar(1, theta)
	if p.add(u.scale(step)).norm() <= radius {
		return step
	}
	// The step s at which |p + s·u| = radius, p being inside.
	pu, inner := p.dot(u), radius*radius-p.dot(p)
	s := -pu + math.Sqrt(math.Max(0, pu*pu+inner))
	return math.Max(0, math.Min(step, s*(1-1e-9)))
}

// botHunter cuts off the runner it sees live.
type botHunter struct {
	cfg *Config
}

// next returns the bot hunter's target for its step at now, from self, given
// the runner's latest validated position and velocity. It aims where the
// runner will be when the hunter could get there (up to BotLeadMax ahead,
// inside the arena) and, while still far, at a point BotPinOffset nearer
// the centre than that, so it comes from the inside and drives the runner
// towards the shrinking edge.
func (b botHunter) next(self motion, now time.Time, radius float64, runner, runnerVel vec) vec {
	c := b.cfg
	dist := self.pos.dist(runner)
	lead := math.Min(dist/c.VHunter, c.BotLeadMax.Seconds())
	aim, _ := intoArena(runner.add(runnerVel.scale(lead)), radius)
	if dist > 3*c.DCapture {
		aim = aim.sub(aim.unit().scale(c.BotPinOffset))
	}
	to := aim.sub(self.pos)
	step := c.VHunter * botDt(c, self, now)
	if n := to.norm(); n <= step {
		return aim
	}
	return self.pos.add(to.unit().scale(step))
}

// unit returns a scaled to length 1, or the zero vector for a zero a.
func (a vec) unit() vec {
	n := a.norm()
	if n == 0 {
		return vec{}
	}
	return a.scale(1 / n)
}

// liveView is what a hunter sees of a runner: the latest validated position
// and the velocity between the last two.
func liveView(runner *ring) (pos, vel vec) {
	last, ok := runner.last()
	if !ok {
		return vec{}, vec{}
	}
	if runner.len() < 2 {
		return last.p, vec{}
	}
	prev := runner.at(runner.len() - 2)
	if dt := last.ts.Sub(prev.ts).Seconds(); dt > 0 {
		vel = last.p.sub(prev.p).scale(1 / dt)
	}
	return last.p, vel
}
