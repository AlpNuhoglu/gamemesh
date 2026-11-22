package player

import (
	"context"
	"errors"
	"strconv"
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
	owner       map[string]string
	existsCalls int32
	failExists  bool
}

func newCountingStore() *countingStore {
	return &countingStore{present: map[string]bool{}, owner: map[string]string{}}
}

func (s *countingStore) Save(_ context.Context, jti, playerID string, _ time.Duration) error {
	s.mu.Lock()
	s.present[jti] = true
	s.owner[jti] = playerID
	s.mu.Unlock()
	return nil
}

func (s *countingStore) DeleteAll(_ context.Context, playerID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var jtis []string
	for jti, owner := range s.owner {
		if owner == playerID {
			jtis = append(jtis, jti)
			delete(s.present, jti)
			delete(s.owner, jti)
		}
	}
	return jtis, nil
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

func (s *countingStore) ExistsMany(ctx context.Context, jtis []string) ([]bool, error) {
	out := make([]bool, len(jtis))
	for i, jti := range jtis {
		ok, err := s.Exists(ctx, jti)
		if err != nil {
			return nil, err
		}
		out[i] = ok
	}
	return out, nil
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

func TestSessionStoreExistsMany(t *testing.T) {
	mr := miniredis.RunT(t)
	store := NewSessionStore(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "a", "p1", time.Hour))
	require.NoError(t, store.Save(ctx, "c", "p3", time.Hour))

	got, err := store.ExistsMany(ctx, []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false, true}, got)

	mr.Close()
	_, err = store.ExistsMany(ctx, []string{"a"})
	assert.Error(t, err)
}

// newIndexedStore is the Redis store on miniredis with a clock the test moves.
func newIndexedStore(t *testing.T) (*redisSessionStore, *miniredis.Miniredis, *redis.Client, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	store := &redisSessionStore{rdb: rdb, now: func() time.Time { return now }}
	return store, mr, rdb, &now
}

func TestSessionStoreDeleteAll(t *testing.T) {
	store, mr, _, _ := newIndexedStore(t)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "jti-a", "player-1", time.Hour))
	require.NoError(t, store.Save(ctx, "jti-b", "player-1", time.Hour))
	require.NoError(t, store.Save(ctx, "jti-other", "player-2", time.Hour))

	jtis, err := store.DeleteAll(ctx, "player-1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"jti-a", "jti-b"}, jtis)

	alive, err := store.ExistsMany(ctx, []string{"jti-a", "jti-b", "jti-other"})
	require.NoError(t, err)
	assert.Equal(t, []bool{false, false, true}, alive,
		"every session of the player is revoked, and nobody else's")
	assert.False(t, mr.Exists(playerSessionsKey("player-1")), "the index outlived its sessions")

	// Nothing left to revoke: not an error.
	jtis, err = store.DeleteAll(ctx, "player-1")
	require.NoError(t, err)
	assert.Empty(t, jtis)
}

// A logout takes its session out of the index, so the index lists live
// sessions only.
func TestSessionStoreDeleteUpdatesTheIndex(t *testing.T) {
	store, _, rdb, _ := newIndexedStore(t)
	ctx := context.Background()
	require.NoError(t, store.Save(ctx, "jti-a", "player-1", time.Hour))
	require.NoError(t, store.Save(ctx, "jti-b", "player-1", time.Hour))

	require.NoError(t, store.Delete(ctx, "jti-a"))
	listed, err := rdb.ZRange(ctx, playerSessionsKey("player-1"), 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{"jti-b"}, listed)

	// Deleting a session that is already gone is a logout repeated.
	require.NoError(t, store.Delete(ctx, "jti-a"))
}

// A guest renews its token every two hours for weeks; the index must not
// keep an entry for each of them.
func TestSessionIndexDropsExpiredSessions(t *testing.T) {
	store, _, rdb, now := newIndexedStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		require.NoError(t, store.Save(ctx, "jti-"+strconv.Itoa(i), "player-1", 2*time.Hour))
		*now = now.Add(3 * time.Hour) // the session saved above has expired
	}
	require.NoError(t, store.Save(ctx, "jti-live", "player-1", 2*time.Hour))

	listed, err := rdb.ZRange(ctx, playerSessionsKey("player-1"), 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{"jti-live"}, listed)
}

// The index must be there for as long as any session it lists: a shorter
// session saved later must not shorten it, a longer one must extend it.
func TestSessionIndexOutlivesItsLongestSession(t *testing.T) {
	t.Run("shorter session second", func(t *testing.T) {
		store, mr, _, _ := newIndexedStore(t)
		ctx := context.Background()
		require.NoError(t, store.Save(ctx, "jti-long", "player-1", 24*time.Hour))
		require.NoError(t, store.Save(ctx, "jti-short", "player-1", 2*time.Hour))
		assert.Equal(t, 24*time.Hour, mr.TTL(playerSessionsKey("player-1")))
	})
	t.Run("longer session second", func(t *testing.T) {
		store, mr, _, _ := newIndexedStore(t)
		ctx := context.Background()
		require.NoError(t, store.Save(ctx, "jti-short", "player-1", 2*time.Hour))
		require.NoError(t, store.Save(ctx, "jti-long", "player-1", 24*time.Hour))
		assert.Equal(t, 24*time.Hour, mr.TTL(playerSessionsKey("player-1")))
	})
}

// Deleting an account on this replica must not leave its sessions usable
// here for the cache's lifetime.
func TestCachedSessionStore_DeleteAllInvalidatesImmediately(t *testing.T) {
	inner := newCountingStore()
	cache := NewCachedSessionStore(inner, time.Minute)
	ctx := context.Background()
	require.NoError(t, cache.Save(ctx, "jti-a", "p1", time.Hour))
	require.NoError(t, cache.Save(ctx, "jti-b", "p1", time.Hour))
	require.NoError(t, cache.Save(ctx, "jti-other", "p2", time.Hour))

	jtis, err := cache.DeleteAll(ctx, "p1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"jti-a", "jti-b"}, jtis)

	for jti, want := range map[string]bool{"jti-a": false, "jti-b": false, "jti-other": true} {
		ok, err := cache.Exists(ctx, jti)
		require.NoError(t, err)
		assert.Equal(t, want, ok, jti)
	}
}
