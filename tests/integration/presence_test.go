//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/alpnuhoglu/gamemesh/internal/presence"
	"github.com/alpnuhoglu/gamemesh/pkg/events"
)

// noopPublisher satisfies events.Publisher without recording — these tests
// assert on Redis state, not the event stream (that is covered by unit tests).
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, string, events.Event) error { return nil }
func (noopPublisher) Close() error                                        { return nil }

// newPresenceService wires a presence Service against the real Redis container
// with a short TTL so real TTL expiry can be observed without long waits.
func newPresenceService(t *testing.T, ttl time.Duration) *presence.Service {
	t.Helper()
	rdb := startRedis(t)
	repo := presence.NewRepository(rdb, ttl)
	return presence.NewService(repo, noopPublisher{}, nil, zap.NewNop())
}

func TestPresenceMultiDeviceAgainstRealRedis(t *testing.T) {
	svc := newPresenceService(t, 45*time.Second)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := svc.Connect(ctx, "alice")
		require.NoError(t, err)
	}
	rec, err := svc.Get(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, 3, rec.ConnectionCount)
	assert.Equal(t, presence.StateOnline, rec.State)

	for i := 0; i < 2; i++ {
		_, err = svc.Disconnect(ctx, "alice")
		require.NoError(t, err)
	}
	rec, err = svc.Get(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, presence.StateOnline, rec.State)

	rec, err = svc.Disconnect(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, presence.StateOffline, rec.State)
}

func TestPresenceTTLExpiryAgainstRealRedis(t *testing.T) {
	// 1s TTL: after heartbeats stop, the key really expires in Redis and the
	// player is reported OFFLINE — crash recovery with no explicit disconnect.
	svc := newPresenceService(t, 1*time.Second)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "alice")
	require.NoError(t, err)

	rec, err := svc.Get(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, presence.StateOnline, rec.State)

	require.Eventually(t, func() bool {
		r, err := svc.Get(ctx, "alice")
		return err == nil && r.State == presence.StateOffline
	}, 5*time.Second, 100*time.Millisecond, "presence should expire to OFFLINE after TTL")
}

func TestPresenceHeartbeatKeepsAliveAgainstRealRedis(t *testing.T) {
	svc := newPresenceService(t, 2*time.Second)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "alice")
	require.NoError(t, err)

	// Beat every 500ms for ~3s; the player must stay ONLINE the whole time even
	// though the TTL is only 2s.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, err := svc.Heartbeat(ctx, "alice")
		require.NoError(t, err)
		rec, err := svc.Get(ctx, "alice")
		require.NoError(t, err)
		require.Equal(t, presence.StateOnline, rec.State)
		time.Sleep(500 * time.Millisecond)
	}
}

func TestPresenceFriendsBatchAgainstRealRedis(t *testing.T) {
	svc := newPresenceService(t, 45*time.Second)
	ctx := context.Background()

	_, err := svc.Connect(ctx, "f1")
	require.NoError(t, err)
	_, err = svc.Connect(ctx, "f2")
	require.NoError(t, err)
	_, err = svc.SetState(ctx, "f2", presence.StateInQueue)
	require.NoError(t, err)

	friends, err := svc.Friends(ctx, []string{"f1", "f2", "f3"})
	require.NoError(t, err)
	require.Len(t, friends, 3)

	byID := map[string]presence.Friend{}
	for _, f := range friends {
		byID[f.PlayerID] = f
	}
	assert.Equal(t, presence.StateOnline, byID["f1"].State)
	assert.Equal(t, presence.StateInQueue, byID["f2"].State)
	assert.Equal(t, presence.StateOffline, byID["f3"].State)
}
