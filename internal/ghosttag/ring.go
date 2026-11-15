package ghosttag

import (
	"math"
	"time"
)

// vec is a point or a displacement in the arena. The centre is the origin.
type vec struct{ X, Y float64 }

func (a vec) add(b vec) vec        { return vec{a.X + b.X, a.Y + b.Y} }
func (a vec) sub(b vec) vec        { return vec{a.X - b.X, a.Y - b.Y} }
func (a vec) scale(k float64) vec  { return vec{a.X * k, a.Y * k} }
func (a vec) dot(b vec) float64    { return a.X*b.X + a.Y*b.Y }
func (a vec) norm() float64        { return math.Hypot(a.X, a.Y) }
func (a vec) dist(b vec) float64   { return a.sub(b).norm() }
func lerp(a, b vec, f float64) vec { return a.add(b.sub(a).scale(f)) }

// sample is one validated position and the ServerTs it was validated at.
type sample struct {
	ts time.Time
	p  vec
}

// ring is a player's recent validated positions, oldest first, in a fixed
// array (section 1.3). Samples are pushed in ServerTs order; the session
// loop guarantees ServerTs never goes backwards within a room. The capture
// check and the bot runner's delayed sight both read it.
type ring struct {
	buf      []sample
	start, n int
}

func newRing(size int) ring { return ring{buf: make([]sample, size)} }

func (r *ring) reset() { r.start, r.n = 0, 0 }

func (r *ring) len() int { return r.n }

// at returns the i-th sample, 0 being the oldest.
func (r *ring) at(i int) sample { return r.buf[(r.start+i)%len(r.buf)] }

// push appends s, overwriting the oldest sample when the ring is full.
func (r *ring) push(s sample) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = s
		r.n++
		return
	}
	r.buf[r.start] = s
	r.start = (r.start + 1) % len(r.buf)
}

// last returns the newest sample.
func (r *ring) last() (sample, bool) {
	if r.n == 0 {
		return sample{}, false
	}
	return r.at(r.n - 1), true
}

// atOrBefore returns the newest sample whose ServerTs is not after t. It
// never looks past t, so a caller that passes an old t learns nothing newer.
func (r *ring) atOrBefore(t time.Time) (sample, bool) {
	for i := r.n - 1; i >= 0; i-- {
		if s := r.at(i); !s.ts.After(t) {
			return s, true
		}
	}
	return sample{}, false
}

// minDistance returns the shortest distance between p and the path other
// followed during the window [t−w, t], and false if other has no position
// for any part of the window.
//
// The path is piecewise linear between consecutive samples (section 1.3).
// Three details decide captures:
//
//   - The segment that enters the window, from the last sample before t−w
//     to the first one inside it, counts from the point where it crosses
//     t−w (the implementation note of section 1.3). Skipping it would see a
//     capture a sample late; counting it whole would see one that happened
//     before the window.
//   - Nothing after t is used: there is no forward window. In the room no
//     sample is ever newer than the packet being checked, so this only
//     matters as a guarantee.
//   - A player's newest position holds until their next packet. It is the
//     server's current belief about where they are, not a prediction: a
//     runner who stops sending stays where they were last validated, and
//     can be caught there. Section 2 relies on the same model when it
//     pauses the capture check during DISCONNECT_GRACE, because the absent
//     player stands at P_anchor.
func minDistance(other *ring, p vec, t time.Time, w time.Duration) (float64, bool) {
	lo := t.Add(-w)
	i := other.n - 1
	for i >= 0 && other.at(i).ts.After(t) {
		i--
	}
	newest := i // the newest sample not after t
	for i >= 0 && other.at(i).ts.After(lo) {
		i--
	}
	first := i + 1 // the oldest sample inside (lo, t]

	best := math.Inf(1)
	var prev vec
	have := false
	if i >= 0 { // a sample at or before lo: where other was when the window opened
		base := other.at(i)
		prev = base.p
		if first <= newest {
			next := other.at(first)
			if span := next.ts.Sub(base.ts); span > 0 {
				prev = lerp(base.p, next.p, float64(lo.Sub(base.ts))/float64(span))
			}
		}
		best = p.dist(prev)
		have = true
	}
	for j := first; j <= newest; j++ {
		s := other.at(j)
		if have {
			best = math.Min(best, segmentDistance(p, prev, s.p))
		} else {
			best = p.dist(s.p)
		}
		prev, have = s.p, true
	}
	return best, have
}

// segmentDistance is the distance from p to the segment ab.
func segmentDistance(p, a, b vec) float64 {
	ab := b.sub(a)
	l2 := ab.dot(ab)
	if l2 == 0 {
		return p.dist(a)
	}
	f := p.sub(a).dot(ab) / l2
	f = math.Max(0, math.Min(1, f))
	return p.dist(a.add(ab.scale(f)))
}
