package ghosttag

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The session's rarer paths. Like the other session tests, each waits on
// something observable (a recorded message, a channel, Done) and asserts an
// absence only after Done.

// blockingStore blocks in Settle until its context is done, after closing
// entered, and records why the context ended.
type blockingStore struct {
	entered chan struct{}
	once    sync.Once
	calls   atomic.Int32
	err     atomic.Value // error
}

func newBlockingStore() *blockingStore { return &blockingStore{entered: make(chan struct{})} }

func (s *blockingStore) Settle(ctx context.Context, _ Match, _ func(map[string]Standing) map[string]Award) (Settlement, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	s.err.Store(ctx.Err())
	return Settlement{}, ctx.Err()
}

func lifecycleRig(t *testing.T, cfg Config, tr *fakeTransport, store ScoreStore) (*Session, *syncCounts) {
	t.Helper()
	obs := newSyncCounts()
	s, err := NewSession(cfg, pvp(), tr, store, obs)
	require.NoError(t, err)
	return s, obs
}

func waitClosed(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestSessionSettleTimeout: a store that never answers is cut off by
// SettleTimeout. It sees its context's deadline pass, the failure is
// counted, and the players get the result unranked with zero deltas.
func TestSessionSettleTimeout(t *testing.T) {
	cfg := fastConfig()
	cfg.SettleTimeout = 50 * time.Millisecond
	store := newBlockingStore()
	tr := newFakeTransport("a", "b")
	s, obs := lifecycleRig(t, cfg, tr, store)
	go s.Run(context.Background())

	tr.waitFor(t, "match_end", func(ms []sent) bool { return countType(ms, "match_end") == 1 })
	waitClosed(t, s.Done(), "Done")

	err, _ := store.err.Load().(error)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "the store saw %v", err)
	assert.Equal(t, 1, obs.get(EventSettleFailed))
	end := tr.ofType("match_end")[0].body
	assert.Equal(t, false, end["ranked"])
	assert.Equal(t, map[string]any{"a": 0.0, "b": 0.0}, end["deltas"])
}

// TestSessionCancelledDuringSettle: shutdown while the store is working.
// Run's context is cancelled only once the store has been entered; the
// session then ends with no match_end and no settle failure counted.
func TestSessionCancelledDuringSettle(t *testing.T) {
	store := newBlockingStore()
	tr := newFakeTransport("a", "b")
	s, obs := lifecycleRig(t, fastConfig(), tr, store)
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	waitClosed(t, store.entered, "the store to be entered")
	cancel()
	waitClosed(t, s.Done(), "Done")

	err, _ := store.err.Load().(error)
	assert.True(t, errors.Is(err, context.Canceled), "the store saw %v", err)
	assert.Empty(t, tr.ofType("match_end"))
	assert.Zero(t, obs.get(EventSettleFailed))
	assert.Equal(t, int32(1), store.calls.Load())
}

// TestSessionSecondRunReturnsAtOnce: a second Run while the first is
// playing returns without touching the room; the first goes on to a normal
// end with one match_end, and Done closes once.
func TestSessionSecondRunReturnsAtOnce(t *testing.T) {
	cfg := fastConfig()
	cfg.THalf = time.Hour // the match ends only by the forfeit below
	tr := newFakeTransport("a", "b")
	s, obs := lifecycleRig(t, cfg, tr, nil)
	go s.Run(context.Background())
	tr.waitFor(t, "HALF_1_ACTIVE", hasSnap("HALF_1_ACTIVE"))

	second := make(chan struct{})
	go func() {
		s.Run(context.Background())
		close(second)
	}()
	waitClosed(t, second, "the second Run to return")
	select {
	case <-s.Done():
		t.Fatal("the first Run ended: nothing should have ended it yet")
	default:
	}

	tr.setConnected("b", false)
	s.Notify("b")
	waitClosed(t, s.Done(), "Done")
	assert.Len(t, tr.ofType("match_end"), 1)
	assert.Equal(t, 1, obs.get(EventForfeit))
}

// TestSessionNotifyIsLossless: with no presence buffer at all, notifications
// sent before Run starts wait for it instead of being dropped, and every one
// is delivered: each makes the session send the player the state.
func TestSessionNotifyIsLossless(t *testing.T) {
	cfg := fastConfig()
	cfg.PresenceBuffer = 0
	cfg.THalf = time.Hour
	tr := newFakeTransport("a", "b")
	s, _ := lifecycleRig(t, cfg, tr, nil)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Notify("a")
		}()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	toA := func(ms []sent) int {
		k := 0
		for _, m := range ms {
			if m.how == "to" && m.who == "a" && m.typ() == "snap" {
				k++
			}
		}
		return k
	}
	tr.waitFor(t, "a snap per notification", func(ms []sent) bool { return toA(ms) == n })
	notified := make(chan struct{})
	go func() { wg.Wait(); close(notified) }()
	waitClosed(t, notified, "every Notify to return")
	cancel()
	waitClosed(t, s.Done(), "Done")
	assert.Equal(t, n, toA(tr.snapshot()))
}
