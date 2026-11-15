package ghosttag

import (
	"errors"
	"math"
	"time"
)

// role is a player's part in the current half.
type role uint8

const (
	roleNone role = iota
	roleHunter
	roleRunner
)

// String is the role as the wire spells it.
func (r role) String() string {
	switch r {
	case roleHunter:
		return "HUNTER"
	case roleRunner:
		return "RUNNER"
	}
	return ""
}

func isFinite(f float64) bool    { return !math.IsNaN(f) && !math.IsInf(f, 0) }
func polar(r, theta float64) vec { return vec{r * math.Cos(theta), r * math.Sin(theta)} }

// wrapPi maps an angle into [−π, π].
func wrapPi(a float64) float64 { return math.Remainder(a, 2*math.Pi) }

// motion is what the validation of a player's next packet starts from: the
// last validated position and heading, and the ServerTs they were validated
// at. hasPrev is has_prev_motion of section 1.2; when it is false, pos is the
// spawn point or P_anchor and ts and theta mean nothing.
type motion struct {
	pos     vec
	theta   float64
	ts      time.Time
	hasPrev bool
}

// errNonFinite rejects a position that is NaN or infinite. encoding/json
// cannot produce one, but the bot calls the pipeline too and the pipeline is
// the last line of defence, so it does not rely on its callers.
var errNonFinite = errors.New("ghosttag: non-finite position")

// clipEpsilon is how far a validated position may lie from the raw one and
// still count as unclipped: float rounding in the polar step, not a rule.
const clipEpsilon = 1e-6

// validateMove is the movement validation of section 1.2, the one pipeline
// human packets and bot steps both go through. From m, a player in role r
// asks to be at raw at ServerTs now, in an arena of the given radius. It
// returns the motion after the move and whether the position had to be
// corrected. On an error m is returned unchanged.
//
// The server derives direction and speed from validated positions only; the
// client's vx and vy never enter here.
func validateMove(c *Config, m motion, r role, raw vec, now time.Time, radius float64) (motion, bool, error) {
	d := raw.sub(m.pos)
	dist := d.norm()
	// One check covers NaN and ±Inf in either coordinate (they make the
	// distance NaN or +Inf; math.Hypot(±Inf, NaN) is +Inf) and a finite
	// position so far out that the distance overflows.
	if !isFinite(dist) {
		return m, false, errNonFinite
	}

	// 1. The time step. The first packet after a spawn or a return has no
	// previous packet: it gets one tick's worth (the implementation note of
	// section 1.2), not Δt_max, which would allow a ~90-unit jump.
	dt := c.DtInitial.Seconds()
	if m.hasPrev {
		dt = math.Max(c.DtMin.Seconds(), math.Min(c.DtMax.Seconds(), now.Sub(m.ts).Seconds()))
	}

	if dist == 0 {
		// Standing still: there is no direction to derive and the heading is
		// kept, and a first packet that does not move does not start the
		// motion. The arena still applies: R(t) may have shrunk past a
		// player who stands still.
		if m.hasPrev {
			m.ts = now
		}
		p, scaled := intoArena(m.pos, radius)
		m.pos = p
		return m, scaled, nil
	}

	// 2. Direction, with the runner's turn rate and turn penalty. The hunter
	// turns freely, and so does anyone on their first packet; the heading
	// is recorded either way, for the next packet and the next half.
	thetaRaw := math.Atan2(d.Y, d.X)
	theta := thetaRaw
	allowed := c.VHunter
	if r == roleRunner {
		allowed = c.VRunner
		if m.hasPrev {
			limit := c.OmegaMax * dt
			turn := math.Max(-limit, math.Min(limit, wrapPi(thetaRaw-m.theta)))
			theta = wrapPi(m.theta + turn)
			allowed = c.VRunner * (1 - c.Alpha*math.Abs(turn)/math.Pi)
		}
	}

	// 3. Distance, then the shrinking arena.
	step := math.Min(dist, allowed*dt+c.BJitter)
	p, _ := intoArena(m.pos.add(polar(step, theta)), radius)
	return motion{pos: p, theta: theta, ts: now, hasPrev: true}, p.dist(raw) > clipEpsilon, nil
}

// intoArena scales p onto the boundary circle if it lies outside it, and
// says whether it did.
func intoArena(p vec, radius float64) (vec, bool) {
	if n := p.norm(); n > radius {
		return p.scale(radius / n), true
	}
	return p, false
}

// arenaRadius is R(t) of section 1.2 at elapsed active time into a half: R0
// until the last TShrink of the half, then linear down to RMin.
func arenaRadius(c *Config, elapsed time.Duration) float64 {
	start := c.THalf - c.TShrink
	if elapsed <= start || c.TShrink <= 0 {
		return c.R0
	}
	f := math.Min(1, float64(elapsed-start)/float64(c.TShrink))
	return c.R0 - (c.R0-c.RMin)*f
}
