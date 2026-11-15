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

// next returns the bot runner's target for the tick after self, in an
// arena of the given radius. A fresh reaction delay in
// [BotReactionMin, BotReactionMax] is drawn for every step.
func (b botRunner) next(self motion, radius float64, see sight) vec {
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

	// Near the edge, give up the outward part of the escape and slide along
	// the wall, a little inwards, more so the closer it is.
	if d, edge := self.pos.norm(), radius-c.BotWallMargin; d > edge && d > 0 {
		out := self.pos.scale(1 / d)
		w := math.Min(1, (d-edge)/math.Max(c.BotWallMargin, 1))
		if o := away.dot(out); o > 0 {
			away = away.sub(out.scale(o * w))
		}
		tangent := vec{-out.Y, out.X}
		if away.dot(tangent) < 0 {
			tangent = tangent.scale(-1)
		}
		away = away.add(tangent.scale(w)).sub(out.scale(w / 2)).unit()
	}

	// Turn no faster than a runner may, and pay the turn penalty, so the
	// pipeline has nothing to correct in ordinary play.
	want := math.Atan2(away.Y, away.X)
	theta, turn := want, 0.0
	if self.hasPrev {
		limit := c.OmegaMax * c.Tick.Seconds()
		turn = math.Max(-limit, math.Min(limit, wrapPi(want-self.theta)))
		theta = wrapPi(self.theta + turn)
	}
	step := c.VRunner * (1 - c.Alpha*math.Abs(turn)/math.Pi) * c.Tick.Seconds()
	return self.pos.add(polar(step, theta))
}

// botHunter cuts off the runner it sees live.
type botHunter struct {
	cfg *Config
}

// next returns the bot hunter's target for the tick after self, given the
// runner's latest validated position and velocity. It aims where the
// runner will be when the hunter could get there (up to BotLeadMax ahead,
// inside the arena) and, while still far, at a point BotPinOffset nearer
// the centre than that, so it comes from the inside and drives the runner
// towards the shrinking edge.
func (b botHunter) next(self motion, radius float64, runner, runnerVel vec) vec {
	c := b.cfg
	dist := self.pos.dist(runner)
	lead := math.Min(dist/c.VHunter, c.BotLeadMax.Seconds())
	aim, _ := intoArena(runner.add(runnerVel.scale(lead)), radius)
	if dist > 3*c.DCapture {
		aim = aim.sub(aim.unit().scale(c.BotPinOffset))
	}
	to := aim.sub(self.pos)
	step := c.VHunter * c.Tick.Seconds()
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
