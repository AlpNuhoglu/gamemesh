package wsgateway

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// SessionBatchChecker reports, in order, whether each session still exists.
type SessionBatchChecker interface {
	ExistsMany(ctx context.Context, jtis []string) ([]bool, error)
}

// sweepBatch bounds one Redis pipeline during a sweep.
const sweepBatch = 1000

// RunSessionSweep re-checks every open connection each interval until ctx is
// done. The handshake checks a token once; this keeps that decision true for
// the whole life of the connection: a connection whose session was revoked
// (logout, admin action, expiry in Redis) or whose JWT has expired is closed
// with 1008. It is the safety net under the logout event, which closes
// connections at once but can be missed.
func (h *Hub) RunSessionSweep(ctx context.Context, interval time.Duration, sessions SessionBatchChecker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sweepSessions(ctx, sessions)
		}
	}
}

func (h *Hub) sweepSessions(ctx context.Context, sessions SessionBatchChecker) {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		if c.conn != nil {
			clients = append(clients, c)
		}
	}
	h.mu.RUnlock()

	now := time.Now()
	live := clients[:0]
	for _, c := range clients {
		if !c.expires.IsZero() && now.After(c.expires) {
			c.closeWith(websocket.ClosePolicyViolation, "token expired")
			continue
		}
		live = append(live, c)
	}

	for start := 0; start < len(live); start += sweepBatch {
		batch := live[start:min(start+sweepBatch, len(live))]
		jtis := make([]string, len(batch))
		for i, c := range batch {
			jtis[i] = c.jti
		}
		exists, err := sessions.ExistsMany(ctx, jtis)
		if err != nil {
			// Fail open: dropping every client on a Redis blip would send them
			// all reconnecting into a Redis that is down. The handshake still
			// fails closed, and the next sweep catches up.
			h.log.Warn("session sweep failed, keeping connections", zap.Error(err))
			return
		}
		for i, c := range batch {
			if !exists[i] {
				c.closeWith(websocket.ClosePolicyViolation, "session revoked")
			}
		}
	}
}
