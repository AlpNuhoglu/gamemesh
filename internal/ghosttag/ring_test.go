package ghosttag

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// ringOf builds a ring holding the given samples, each at t0 plus its
// offset in milliseconds.
func ringOf(size int, pts ...struct {
	at   int
	x, y float64
}) *ring {
	r := newRing(size)
	for _, p := range pts {
		r.push(sample{ts: t0.Add(ms(p.at)), p: vec{p.x, p.y}})
	}
	return &r
}

type pt = struct {
	at   int
	x, y float64
}

// TestRingWrapsAndKeepsOrder: a full ring overwrites its oldest sample and
// still reads oldest first.
func TestRingWrapsAndKeepsOrder(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		r.push(sample{ts: t0.Add(ms(i * 40)), p: vec{float64(i), 0}})
	}
	require.Equal(t, 3, r.len())
	assert.Equal(t, 2.0, r.at(0).p.X)
	assert.Equal(t, 3.0, r.at(1).p.X)
	assert.Equal(t, 4.0, r.at(2).p.X)
	last, ok := r.last()
	require.True(t, ok)
	assert.Equal(t, 4.0, last.p.X)

	r.reset()
	assert.Equal(t, 0, r.len())
	_, ok = r.last()
	assert.False(t, ok)
}

// TestRingAtOrBeforeNeverLooksAhead: the lookup the bot's sight is built on
// returns the newest sample not after t, never a later one.
func TestRingAtOrBeforeNeverLooksAhead(t *testing.T) {
	r := ringOf(8, pt{0, 0, 0}, pt{40, 1, 0}, pt{80, 2, 0})

	s, ok := r.atOrBefore(t0.Add(ms(79)))
	require.True(t, ok)
	assert.Equal(t, 1.0, s.p.X)
	s, ok = r.atOrBefore(t0.Add(ms(80)))
	require.True(t, ok)
	assert.Equal(t, 2.0, s.p.X)
	_, ok = r.atOrBefore(t0.Add(-time.Millisecond))
	assert.False(t, ok)
}

// TestCaptureCountsTheSegmentEnteringTheWindow: the other player crossed
// the point 140 ms before t, on the segment from a sample 200 ms before
// (outside the window) to one 100 ms before (inside it). Only the entering
// segment carries that crossing; without it the closest the path comes is
// the inside sample, 80 units away.
func TestCaptureCountsTheSegmentEnteringTheWindow(t *testing.T) {
	other := ringOf(8, pt{-200, -100, 0}, pt{-100, 100, 0})

	d, ok := minDistance(other, vec{20, 0}, t0, ms(150))

	require.True(t, ok)
	assert.InDelta(t, 0, d, 1e-9, "the crossing at t−140 ms is inside the window")
}

// TestCaptureClipsTheEnteringSegmentToTheWindow: the same segment, but the
// point it crossed was passed 180 ms before t, outside the window. The
// segment counts only from where it enters the window (x = 0 at t−150 ms),
// so the distance is 60, not 0.
func TestCaptureClipsTheEnteringSegmentToTheWindow(t *testing.T) {
	other := ringOf(8, pt{-200, -100, 0}, pt{-100, 100, 0})

	d, ok := minDistance(other, vec{-60, 0}, t0, ms(150))

	require.True(t, ok)
	assert.InDelta(t, 60, d, 1e-9)
}

// TestCaptureHasNoForwardWindow: a sample newer than t (which the room never
// has, but the function must not depend on that) is not used, even 40 ms
// ahead, inside the t+50 ms the design rejects. The position held from
// before the window is what counts.
func TestCaptureHasNoForwardWindow(t *testing.T) {
	other := ringOf(8, pt{-300, 300, 0}, pt{40, 0, 0})

	d, ok := minDistance(other, vec{0, 0}, t0, ms(150))

	require.True(t, ok)
	assert.InDelta(t, 300, d, 1e-9)
}

// TestCaptureHoldsTheLastPosition: a player whose last packet is older than
// the window is still where it was validated, and can be caught there.
func TestCaptureHoldsTheLastPosition(t *testing.T) {
	other := ringOf(8, pt{-1000, 10, 0})

	d, ok := minDistance(other, vec{0, 0}, t0, ms(150))

	require.True(t, ok)
	assert.InDelta(t, 10, d, 1e-9)

	_, ok = minDistance(ringOf(8), vec{0, 0}, t0, ms(150))
	assert.False(t, ok, "an empty ring gives no distance")
}

// TestCaptureInterpolatesInsideTheWindow: two samples inside the window,
// the path between them passes 5 units from the point.
func TestCaptureInterpolatesInsideTheWindow(t *testing.T) {
	other := ringOf(8, pt{-120, -50, 5}, pt{-80, 50, 5}, pt{-40, 50, 100})

	d, ok := minDistance(other, vec{0, 0}, t0, ms(150))

	require.True(t, ok)
	assert.InDelta(t, 5, d, 1e-9)
}
