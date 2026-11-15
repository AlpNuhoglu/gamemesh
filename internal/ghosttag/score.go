package ghosttag

import (
	"math"
	"time"
)

// outcome is how a match ended.
type outcome uint8

const (
	// outcomeCompleted: both halves were played.
	outcomeCompleted outcome = iota + 1
	// outcomeForfeit: one player did not come back within TGrace.
	outcomeForfeit
	// outcomeVoid: both players were gone when the grace ran out. Nobody
	// wins, nothing is settled (section 2).
	outcomeVoid
	// outcomeCancelled: a player did not join within TJoin. The game never
	// started; nothing is settled (section 7).
	outcomeCancelled
)

// tally is what a match leaves about one player.
type tally struct {
	id string
	// captured and captureMs: whether the player caught the runner in the
	// half they hunted, and how far into that half, in active time.
	captured  bool
	captureMs int
	// pressureSum and pressureN: the hunter pressure samples of the half the
	// player hunted (section 1.4). Only the hunter's total grows.
	pressureSum float64
	pressureN   int
}

// meanPressure is the player's mean distance to the runner while hunting,
// +Inf without a sample.
func (t tally) meanPressure() float64 {
	if t.pressureN == 0 {
		return math.Inf(1)
	}
	return t.pressureSum / float64(t.pressureN)
}

// matchResult is the finished match as the room saw it.
type matchResult struct {
	outcome outcome
	// players are in the room's order: the humans as Spec lists them, then
	// the bot.
	players []tally
	// leaver is the player who forfeited.
	leaver string
}

// settles says the match is one the score layer may see: it was played to
// an end. A void or cancelled match never is.
func (res matchResult) settles() bool {
	return res.outcome == outcomeCompleted || res.outcome == outcomeForfeit
}

// verdict is who won and how.
type verdict struct {
	winner    string // "" for a draw and for a match that has no result
	draw      bool
	byCapture bool // decided by a capture, so the speed bonus applies
}

// decide applies the winner order: a capture beats no capture; of two
// captures the faster wins; otherwise the lower mean hunter pressure
// distance wins, and within DrawTolerance it is a draw. A forfeit goes to
// the player who stayed. A void or cancelled match has no winner.
func decide(c *Config, res matchResult) verdict {
	switch res.outcome {
	case outcomeForfeit:
		for _, p := range res.players {
			if p.id != res.leaver {
				return verdict{winner: p.id}
			}
		}
		return verdict{}
	case outcomeCompleted:
	default:
		return verdict{}
	}
	if len(res.players) != 2 {
		return verdict{}
	}
	a, b := res.players[0], res.players[1]
	switch {
	case a.captured && !b.captured:
		return verdict{winner: a.id, byCapture: true}
	case b.captured && !a.captured:
		return verdict{winner: b.id, byCapture: true}
	case a.captured && b.captured && a.captureMs != b.captureMs:
		if a.captureMs < b.captureMs {
			return verdict{winner: a.id, byCapture: true}
		}
		return verdict{winner: b.id, byCapture: true}
	}
	// No decisive capture: the tie-breaker. A player with no sample at all
	// has +Inf; two of those are a draw.
	ma, mb := a.meanPressure(), b.meanPressure()
	if (math.IsInf(ma, 1) && math.IsInf(mb, 1)) || math.Abs(ma-mb) <= c.DrawTolerance {
		return verdict{draw: true}
	}
	if ma < mb {
		return verdict{winner: a.id}
	}
	return verdict{winner: b.id}
}

// scorer returns the scoring rule of section 4 for one finished match, as
// the score callback of ScoreStore.Settle. It is pure: it reads only its
// argument and what it closed over, so the store may call it any number of
// times.
func scorer(c *Config, res matchResult) func(prior map[string]Standing) map[string]Award {
	v := decide(c, res)
	pts := c.Points
	halfMs := int(c.THalf / time.Millisecond)
	unitMs := int(pts.SpeedUnit / time.Millisecond)
	return func(prior map[string]Standing) map[string]Award {
		if !res.settles() {
			return map[string]Award{}
		}
		out := make(map[string]Award, len(res.players))
		for _, p := range res.players {
			streak := prior[p.id].WinStreak
			capMs := 0
			if p.captured {
				capMs = p.captureMs
			}
			a := Award{WinStreak: streak, CaptureMs: capMs}
			switch {
			case res.outcome == outcomeForfeit && p.id == res.leaver:
				a.Delta, a.WinStreak = pts.Forfeit, 0
			case res.outcome == outcomeForfeit:
				// The streak is kept, not extended: a forfeit is not a win
				// the player earned in play.
				a.Delta, a.Win = pts.ForfeitWin, true
			case v.draw:
				a.Delta = pts.Draw
			case p.id == v.winner:
				a.WinStreak = streak + 1 // this win included (section 4)
				a.Delta = pts.Win + min(a.WinStreak*pts.StreakStep, pts.StreakCap)
				if v.byCapture {
					// The winner's own capture, as hunter.
					a.Delta += max(0, halfMs-p.captureMs) / unitMs
				}
				a.Win = true
			default:
				a.Delta, a.WinStreak = pts.Loss, 0
			}
			out[p.id] = a
		}
		return out
	}
}
