package player

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// SessionStore caches active sessions keyed by JWT ID (JTI).
//
// Why Redis strings with TTL: a session is a single small value whose
// lifetime exactly matches the JWT expiry, so SET ... EX gives O(1) writes
// and automatic cleanup — no cron jobs, no table bloat. It also enables
// server-side logout (revocation) for otherwise stateless JWTs.
type SessionStore interface {
	Save(ctx context.Context, jti, playerID string, ttl time.Duration) error
	Delete(ctx context.Context, jti string) error
	Exists(ctx context.Context, jti string) (bool, error)
}

type redisSessionStore struct {
	rdb *redis.Client
}

// NewSessionStore returns the Redis-backed session cache.
func NewSessionStore(rdb *redis.Client) SessionStore {
	return &redisSessionStore{rdb: rdb}
}

func sessionKey(jti string) string { return "session:" + jti }

func (s *redisSessionStore) Save(ctx context.Context, jti, playerID string, ttl time.Duration) error {
	return s.rdb.Set(ctx, sessionKey(jti), playerID, ttl).Err()
}

func (s *redisSessionStore) Delete(ctx context.Context, jti string) error {
	return s.rdb.Del(ctx, sessionKey(jti)).Err()
}

func (s *redisSessionStore) Exists(ctx context.Context, jti string) (bool, error) {
	n, err := s.rdb.Exists(ctx, sessionKey(jti)).Result()
	return n > 0, err
}

// cachedSessionStore wraps a SessionStore with a short-lived, in-process
// positive cache so the gateway can enforce JWT revocation on every
// authenticated request without hitting Redis each time.
//
// Positive cache: a "valid" verdict for a JTI is trusted for ttl (e.g. 5s)
// before Redis is consulted again. That ttl is the worst-case window a revoked
// token stays usable on this replica — an accepted eventual-consistency
// trade-off that avoids the multi-replica complexity of a synchronised
// revocation (negative) list. Delete drops the local entry immediately, so a
// logout served by the same replica revokes at once.
//
// Concurrency: the access pattern is read-mostly (one Redis lookup per JTI per
// ttl window, then many lock-free-ish RLock reads), so a single RWMutex is
// ample — at the gateway's RPS and CPU budget a sharded map would be
// over-engineering. Because this is a decorator behind the SessionStore
// interface, the internal map can be swapped for shards later WITHOUT touching
// any caller if profiling ever shows contention. The RWMutex + map + janitor
// shape deliberately mirrors middleware.ipLimiter for codebase cohesion.
type cachedSessionStore struct {
	inner SessionStore
	ttl   time.Duration

	mu      sync.RWMutex
	entries map[string]sessionEntry
}

type sessionEntry struct {
	valid     bool
	expiresAt time.Time
}

// NewCachedSessionStore wraps store with an in-process positive cache of the
// given ttl and starts a background janitor that evicts expired entries.
func NewCachedSessionStore(store SessionStore, ttl time.Duration) SessionStore {
	c := &cachedSessionStore{
		inner:   store,
		ttl:     ttl,
		entries: make(map[string]sessionEntry),
	}
	go c.janitor()
	return c
}

// Save delegates to Redis and seeds the local cache so the just-created session
// is immediately served from memory (a login is normally followed by requests).
func (c *cachedSessionStore) Save(ctx context.Context, jti, playerID string, ttl time.Duration) error {
	if err := c.inner.Save(ctx, jti, playerID, ttl); err != nil {
		return err
	}
	c.set(jti, true)
	return nil
}

// Delete removes the session from Redis AND evicts the local entry so a logout
// handled by this replica takes effect at once (no ttl wait on this node).
func (c *cachedSessionStore) Delete(ctx context.Context, jti string) error {
	c.evict(jti)
	return c.inner.Delete(ctx, jti)
}

// Exists serves a cached verdict when fresh; otherwise it consults Redis once
// and caches the result for ttl.
func (c *cachedSessionStore) Exists(ctx context.Context, jti string) (bool, error) {
	if v, ok := c.get(jti); ok {
		return v, nil
	}
	exists, err := c.inner.Exists(ctx, jti)
	if err != nil {
		// Fail open is unsafe for revocation; surface the error and let the
		// caller decide (the middleware rejects on error).
		return false, err
	}
	c.set(jti, exists)
	return exists, nil
}

func (c *cachedSessionStore) get(jti string) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[jti]
	if !ok || time.Now().After(e.expiresAt) {
		return false, false
	}
	return e.valid, true
}

func (c *cachedSessionStore) set(jti string, valid bool) {
	c.mu.Lock()
	c.entries[jti] = sessionEntry{valid: valid, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}

func (c *cachedSessionStore) evict(jti string) {
	c.mu.Lock()
	delete(c.entries, jti)
	c.mu.Unlock()
}

// janitor evicts expired entries every ttl so the map cannot grow unbounded
// from one-shot JTIs. Mirrors middleware.ipLimiter.janitor.
func (c *cachedSessionStore) janitor() {
	for range time.Tick(c.ttl) {
		now := time.Now()
		c.mu.Lock()
		// Pre-size the kill list to the current map length: at most every entry
		// is expired, so the slice never grows during collection.
		expired := make([]string, 0, len(c.entries))
		for jti, e := range c.entries {
			if now.After(e.expiresAt) {
				expired = append(expired, jti)
			}
		}
		for _, jti := range expired {
			delete(c.entries, jti)
		}
		c.mu.Unlock()
	}
}
