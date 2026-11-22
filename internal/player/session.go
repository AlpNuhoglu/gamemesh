package player

import (
	"context"
	"errors"
	"strconv"
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
	// ExistsMany reports, in order, whether each session exists, in one round
	// trip. The WebSocket service uses it to re-check every open connection.
	ExistsMany(ctx context.Context, jtis []string) ([]bool, error)
	// DeleteAll revokes every session of a player and returns their JTIs. It
	// is what makes deleting an account final: without it the account's other
	// tokens would stay valid until they expire.
	DeleteAll(ctx context.Context, playerID string) ([]string, error)
}

type redisSessionStore struct {
	rdb *redis.Client
	now func() time.Time
}

// NewSessionStore returns the Redis-backed session cache.
func NewSessionStore(rdb *redis.Client) SessionStore {
	return &redisSessionStore{rdb: rdb, now: time.Now}
}

func sessionKey(jti string) string { return "session:" + jti }

// playerSessionsKey is the index of a player's sessions: a sorted set of
// JTIs scored by the Unix second each session expires. A session key is
// found by its JTI only, so without the index "every session of this player"
// would be a scan of the whole keyspace.
func playerSessionsKey(playerID string) string { return "sessions:" + playerID }

// Save stores the session and lists it in the player's index.
//
// The index is kept bounded by dropping, on every save, the entries whose
// session has already expired, and it outlives its longest session: its own
// expiry is only ever moved later (NX sets it when the key is new, GT when
// the new session ends after the ones already listed).
func (s *redisSessionStore) Save(ctx context.Context, jti, playerID string, ttl time.Duration) error {
	now := s.now()
	index := playerSessionsKey(playerID)
	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, sessionKey(jti), playerID, ttl)
		p.ZRemRangeByScore(ctx, index, "-inf", strconv.FormatInt(now.Unix(), 10))
		p.ZAdd(ctx, index, redis.Z{Score: float64(now.Add(ttl).Unix()), Member: jti})
		p.ExpireNX(ctx, index, ttl)
		p.ExpireGT(ctx, index, ttl)
		return nil
	})
	return err
}

// Delete revokes one session and takes it out of its player's index.
func (s *redisSessionStore) Delete(ctx context.Context, jti string) error {
	playerID, err := s.rdb.GetDel(ctx, sessionKey(jti)).Result()
	if errors.Is(err, redis.Nil) {
		return nil // already gone: expired, or revoked before
	}
	if err != nil {
		return err
	}
	return s.rdb.ZRem(ctx, playerSessionsKey(playerID), jti).Err()
}

func (s *redisSessionStore) DeleteAll(ctx context.Context, playerID string) ([]string, error) {
	index := playerSessionsKey(playerID)
	jtis, err := s.rdb.ZRange(ctx, index, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(jtis)+1)
	for _, jti := range jtis {
		keys = append(keys, sessionKey(jti))
	}
	keys = append(keys, index)
	if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
		return nil, err
	}
	return jtis, nil
}

func (s *redisSessionStore) Exists(ctx context.Context, jti string) (bool, error) {
	n, err := s.rdb.Exists(ctx, sessionKey(jti)).Result()
	return n > 0, err
}

func (s *redisSessionStore) ExistsMany(ctx context.Context, jtis []string) ([]bool, error) {
	pipe := s.rdb.Pipeline()
	cmds := make([]*redis.IntCmd, len(jtis))
	for i, jti := range jtis {
		cmds[i] = pipe.Exists(ctx, sessionKey(jti))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([]bool, len(jtis))
	for i, cmd := range cmds {
		out[i] = cmd.Val() > 0
	}
	return out, nil
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

// DeleteAll revokes the player's sessions in Redis and evicts them here, so
// on this replica they stop working at once.
func (c *cachedSessionStore) DeleteAll(ctx context.Context, playerID string) ([]string, error) {
	jtis, err := c.inner.DeleteAll(ctx, playerID)
	for _, jti := range jtis {
		c.evict(jti)
	}
	return jtis, err
}

// ExistsMany bypasses the cache: a batch re-check wants Redis' current view.
func (c *cachedSessionStore) ExistsMany(ctx context.Context, jtis []string) ([]bool, error) {
	return c.inner.ExistsMany(ctx, jtis)
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
