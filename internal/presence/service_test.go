package presence

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

const testTTL = 45 * time.Second

// capturingPublisher records every published event so tests can assert on the
// presence event stream. Safe for concurrent use.
type capturingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *capturingPublisher) Publish(_ context.Context, _ string, e events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}

func (p *capturingPublisher) Close() error { return nil }

// reset clears recorded events so a test can assert only on events that occur
// after setup (e.g. after the initial connects).
func (p *capturingPublisher) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = nil
}

func (p *capturingPublisher) types() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.events))
	for i, e := range p.events {
		out[i] = e.Type
	}
	return out
}

func newTestService(t *testing.T) (*Service, *capturingPublisher, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	pub := &capturingPublisher{}
	svc := NewService(NewRepository(rdb, testTTL), pub, nil, zap.NewNop())
	return svc, pub, mr
}

// mustConnect connects each player once, failing the test on any error.
func mustConnect(ctx context.Context, t *testing.T, svc *Service, ids ...string) error {
	t.Helper()
	for _, id := range ids {
		if _, err := svc.Connect(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func TestConnectGoesOnlineAndEmitsOnline(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	rec, err := svc.Connect(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, StateOnline, rec.State)
	assert.Equal(t, 1, rec.ConnectionCount)
	assert.Equal(t, []string{events.TypePresenceOnline}, pub.types())
}

func TestMultiDeviceStaysOnlineUntilLastDisconnect(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	// Three devices connect.
	for i := 0; i < 3; i++ {
		_, err := svc.Connect(ctx, "alice")
		require.NoError(t, err)
	}
	rec, err := svc.Get(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, 3, rec.ConnectionCount)
	assert.Equal(t, StateOnline, rec.State)

	// First two disconnects keep the player ONLINE.
	for i := 0; i < 2; i++ {
		rec, err = svc.Disconnect(ctx, "alice")
		require.NoError(t, err)
		assert.Equal(t, StateOnline, rec.State)
	}

	// Last disconnect → OFFLINE.
	rec, err = svc.Disconnect(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, StateOffline, rec.State)
	assert.Equal(t, 0, rec.ConnectionCount)

	// Exactly one PresenceOnline (first connect) and one PresenceOffline (last
	// disconnect) — the middle connects/disconnects do not change state, so no
	// extra events.
	assert.Equal(t, []string{events.TypePresenceOnline, events.TypePresenceOffline}, pub.types())
}

func TestHeartbeatRefreshesTTL(t *testing.T) {
	svc, _, mr := newTestService(t)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "alice")
	require.NoError(t, err)
	assert.InDelta(t, testTTL.Seconds(), mr.TTL(key("alice")).Seconds(), 1)

	// Age the key partway, then heartbeat and confirm the TTL is reset.
	mr.FastForward(30 * time.Second)
	assert.Less(t, mr.TTL(key("alice")).Seconds(), testTTL.Seconds())

	_, err = svc.Heartbeat(ctx, "alice")
	require.NoError(t, err)
	assert.InDelta(t, testTTL.Seconds(), mr.TTL(key("alice")).Seconds(), 1)
}

func TestTTLExpiryReportsOfflineWithoutEvents(t *testing.T) {
	svc, pub, mr := newTestService(t)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "alice")
	require.NoError(t, err)

	// Simulate stopped heartbeats: let the key expire.
	mr.FastForward(testTTL + time.Second)

	rec, err := svc.Get(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, StateOffline, rec.State)

	// Reads are side-effect free: no PresenceOffline emitted on expiry/read.
	assert.Equal(t, []string{events.TypePresenceOnline}, pub.types(),
		"only the connect event should exist; reads must not publish")
}

func TestHeartbeatRecreatesExpiredRecord(t *testing.T) {
	svc, _, mr := newTestService(t)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "alice")
	require.NoError(t, err)
	mr.FastForward(testTTL + time.Second) // crash: key gone

	// A resumed heartbeat self-heals presence back to ONLINE.
	rec, err := svc.Heartbeat(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, StateOnline, rec.State)
	assert.Equal(t, 1, rec.ConnectionCount)
}

func TestHeartbeatManyRefreshesTTLWithoutEvents(t *testing.T) {
	svc, pub, mr := newTestService(t)
	ctx := context.Background()

	require.NoError(t, mustConnect(ctx, t, svc, "a", "b", "c"))
	pub.reset()

	// Age all keys partway, then bulk-heartbeat and confirm TTLs are reset and
	// no events fire (steady-state refresh of existing records).
	mr.FastForward(30 * time.Second)
	require.NoError(t, svc.HeartbeatMany(ctx, []string{"a", "b", "c"}))

	for _, id := range []string{"a", "b", "c"} {
		assert.InDelta(t, testTTL.Seconds(), mr.TTL(key(id)).Seconds(), 1)
	}
	assert.Empty(t, pub.types(), "refreshing live records must not publish")
}

func TestHeartbeatManyHealsExpiredRecords(t *testing.T) {
	svc, pub, mr := newTestService(t)
	ctx := context.Background()

	require.NoError(t, mustConnect(ctx, t, svc, "alive", "crashed"))
	pub.reset()

	// "crashed" expires (heartbeats stopped); "alive" stays within TTL by being
	// re-created just before expiry below.
	mr.FastForward(testTTL + time.Second) // both keys gone now

	// Bulk heartbeat must re-create BOTH as ONLINE (heal pass) and publish a
	// PresenceOnline for each, since both had expired.
	require.NoError(t, svc.HeartbeatMany(ctx, []string{"alive", "crashed"}))

	for _, id := range []string{"alive", "crashed"} {
		rec, err := svc.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, StateOnline, rec.State)
	}
	// Two heals → two PresenceOnline events.
	assert.Equal(t, []string{events.TypePresenceOnline, events.TypePresenceOnline}, pub.types())
}

func TestHeartbeatManyEmptyIsNoop(t *testing.T) {
	svc, pub, _ := newTestService(t)
	require.NoError(t, svc.HeartbeatMany(context.Background(), nil))
	assert.Empty(t, pub.types())
}

func TestSetStateAllowsQueueAndMatchTransitions(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "alice")
	require.NoError(t, err)

	rec, err := svc.SetState(ctx, "alice", StateInQueue)
	require.NoError(t, err)
	assert.Equal(t, StateInQueue, rec.State)

	rec, err = svc.SetState(ctx, "alice", StateInMatch)
	require.NoError(t, err)
	assert.Equal(t, StateInMatch, rec.State)

	rec, err = svc.SetState(ctx, "alice", StateOnline)
	require.NoError(t, err)
	assert.Equal(t, StateOnline, rec.State)

	// online, in_queue, in_match, online → 3 state-changed style events after
	// the initial PresenceOnline.
	assert.Equal(t, []string{
		events.TypePresenceOnline,
		events.TypePresenceStateChanged,
		events.TypePresenceStateChanged,
		events.TypePresenceStateChanged,
	}, pub.types())
}

func TestSetStateRejectsOfflineToActive(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.SetState(ctx, "ghost", StateInQueue)
	require.ErrorIs(t, err, ErrInvalidTransition)

	_, err = svc.SetState(ctx, "ghost", StateInMatch)
	require.ErrorIs(t, err, ErrInvalidTransition)

	// Nothing was published for rejected transitions, and the player stays OFFLINE.
	assert.Empty(t, pub.types())
	rec, err := svc.Get(ctx, "ghost")
	require.NoError(t, err)
	assert.Equal(t, StateOffline, rec.State)
}

func TestSetStateRejectsUnknownState(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.SetState(context.Background(), "alice", State("FLYING"))
	require.ErrorIs(t, err, ErrInvalidTransition)
}

func TestFriendsBulkLookup(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "online1")
	require.NoError(t, err)
	_, err = svc.Connect(ctx, "online2")
	require.NoError(t, err)
	_, err = svc.SetState(ctx, "online2", StateInMatch)
	require.NoError(t, err)

	friends, err := svc.Friends(ctx, []string{"online1", "missing", "online2"})
	require.NoError(t, err)
	require.Len(t, friends, 3)

	byID := map[string]Friend{}
	for _, f := range friends {
		byID[f.PlayerID] = f
	}
	assert.Equal(t, StateOnline, byID["online1"].State)
	assert.Equal(t, StateInMatch, byID["online2"].State)
	// A friend with no live key reads as OFFLINE (no N+1, no error).
	assert.Equal(t, StateOffline, byID["missing"].State)
}

func TestFriendsEmptyInput(t *testing.T) {
	svc, _, _ := newTestService(t)
	friends, err := svc.Friends(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, friends)
}

func TestReconnectRestoresOnline(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "alice")
	require.NoError(t, err)
	rec, err := svc.Disconnect(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, StateOffline, rec.State)

	rec, err = svc.Connect(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, StateOnline, rec.State)
	assert.Equal(t, 1, rec.ConnectionCount)
}

func TestConcurrentConnectsCountCorrectly(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = svc.Connect(ctx, "alice")
		}()
	}
	wg.Wait()

	// The Lua-script read-modify-write must not lose any increment under
	// concurrency: all N connections counted.
	rec, err := svc.Get(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, n, rec.ConnectionCount)
	assert.Equal(t, StateOnline, rec.State)
}
