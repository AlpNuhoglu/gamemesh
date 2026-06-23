package player

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingStore is an in-memory SessionStore that records how many times each
// method hits it, so cache tests can prove Redis was (not) consulted.
type countingStore struct {
	mu          sync.Mutex
	present     map[string]bool
	existsCalls int32
	failExists  bool
}

func newCountingStore() *countingStore {
	return &countingStore{present: map[string]bool{}}
}

func (s *countingStore) Save(_ context.Context, jti, _ string, _ time.Duration) error {
	s.mu.Lock()
	s.present[jti] = true
	s.mu.Unlock()
	return nil
}

func (s *countingStore) Delete(_ context.Context, jti string) error {
	s.mu.Lock()
	delete(s.present, jti)
	s.mu.Unlock()
	return nil
}

func (s *countingStore) Exists(_ context.Context, jti string) (bool, error) {
	atomic.AddInt32(&s.existsCalls, 1)
	if s.failExists {
		return false, errors.New("redis down")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.present[jti], nil
}

func (s *countingStore) calls() int32 { return atomic.LoadInt32(&s.existsCalls) }

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

func TestCachedSessionStore_CacheHitSkipsRedis(t *testing.T) {
	inner := newCountingStore()
	require.NoError(t, inner.Save(context.Background(), "jti", "p1", time.Hour))
	cache := NewCachedSessionStore(inner, time.Minute)
	ctx := context.Background()

	// First lookup misses the cache → hits Redis once.
	ok, err := cache.Exists(ctx, "jti")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int32(1), inner.calls())

	// Subsequent lookups within the TTL are served from cache → no more Redis.
	for i := 0; i < 50; i++ {
		ok, err = cache.Exists(ctx, "jti")
		require.NoError(t, err)
		assert.True(t, ok)
	}
	assert.Equal(t, int32(1), inner.calls(), "cache hits must not hit Redis")
}

func TestCachedSessionStore_TTLReChecksRedis(t *testing.T) {
	inner := newCountingStore()
	require.NoError(t, inner.Save(context.Background(), "jti", "p1", time.Hour))
	cache := NewCachedSessionStore(inner, 20*time.Millisecond)
	ctx := context.Background()

	_, _ = cache.Exists(ctx, "jti")
	assert.Equal(t, int32(1), inner.calls())

	// After the cache entry expires, the next lookup re-checks Redis.
	time.Sleep(40 * time.Millisecond)
	_, _ = cache.Exists(ctx, "jti")
	assert.Equal(t, int32(2), inner.calls(), "expired cache entry must re-check Redis")
}

func TestCachedSessionStore_DeleteInvalidatesImmediately(t *testing.T) {
	inner := newCountingStore()
	require.NoError(t, inner.Save(context.Background(), "jti", "p1", time.Hour))
	cache := NewCachedSessionStore(inner, time.Minute)
	ctx := context.Background()

	// Warm the cache with a "valid" verdict.
	ok, _ := cache.Exists(ctx, "jti")
	require.True(t, ok)

	// Logout on this replica: Delete must evict the local entry at once, so the
	// next check re-reads Redis (now absent) rather than serving the stale cache.
	require.NoError(t, cache.Delete(ctx, "jti"))
	ok, err := cache.Exists(ctx, "jti")
	require.NoError(t, err)
	assert.False(t, ok, "deleted session must not be served from cache")
}

func TestCachedSessionStore_PropagatesError(t *testing.T) {
	inner := newCountingStore()
	inner.failExists = true
	cache := NewCachedSessionStore(inner, time.Minute)

	_, err := cache.Exists(context.Background(), "jti")
	require.Error(t, err, "store errors must surface (caller fails closed)")
}

func TestCachedSessionStore_ConcurrentAccess(t *testing.T) {
	inner := newCountingStore()
	require.NoError(t, inner.Save(context.Background(), "jti", "p1", time.Hour))
	cache := NewCachedSessionStore(inner, 5*time.Millisecond)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = cache.Exists(ctx, "jti")
				if j%5 == 0 {
					_ = cache.Delete(ctx, "jti")
					_ = cache.Save(ctx, "jti", "p1", time.Hour)
				}
			}
		}()
	}
	wg.Wait()
}
