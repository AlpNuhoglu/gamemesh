package player

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionStoreLifecycle(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := NewSessionStore(rdb)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, "jti-1", "player-1", time.Hour))

	exists, err := store.Exists(ctx, "jti-1")
	require.NoError(t, err)
	assert.True(t, exists)

	require.NoError(t, store.Delete(ctx, "jti-1"))
	exists, err = store.Exists(ctx, "jti-1")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestSessionStoreTTLExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := NewSessionStore(rdb)
	ctx := context.Background()

	require.NoError(t, store.Save(ctx, "jti-ttl", "player-1", time.Minute))
	mr.FastForward(2 * time.Minute)

	exists, err := store.Exists(ctx, "jti-ttl")
	require.NoError(t, err)
	assert.False(t, exists, "session must expire with the JWT")
}
