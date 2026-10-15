// Package health provides the heartbeat that background loops use to prove,
// through /livez, that they are still running.
package health

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Heartbeat records when a background loop last finished an iteration.
//
// A loop beats on every iteration, whether or not the work succeeded.
// Liveness asks "is the loop still turning?", not "are its dependencies up?":
// that is readiness' job, and restarting the process would not fix a
// dependency, it would only turn an outage into a restart storm.
type Heartbeat struct {
	last atomic.Int64 // unix nanoseconds of the last beat
}

// NewHeartbeat returns a heartbeat that counts as fresh from now, so a loop
// that has not finished its first iteration yet is not reported as stalled.
func NewHeartbeat() *Heartbeat {
	h := &Heartbeat{}
	h.Beat()
	return h
}

// Beat records an iteration. It is a no-op on a nil Heartbeat, so loops can
// beat unconditionally when none is configured.
func (h *Heartbeat) Beat() {
	if h != nil {
		h.last.Store(time.Now().UnixNano())
	}
}

// Check returns a liveness check (matching server.Check) that fails once the
// last beat is older than maxAge.
func (h *Heartbeat) Check(maxAge time.Duration) func(context.Context) error {
	return func(context.Context) error {
		if age := time.Since(time.Unix(0, h.last.Load())); age > maxAge {
			return fmt.Errorf("no heartbeat for %s (limit %s)", age.Round(time.Millisecond), maxAge)
		}
		return nil
	}
}

// StallAfter is how long a loop with the given interval may go without a beat
// before it counts as stalled: three missed iterations, and never less than
// 30 s, so a slow but healthy iteration is not mistaken for a stuck one.
func StallAfter(interval time.Duration) time.Duration {
	return max(30*time.Second, 3*interval)
}
