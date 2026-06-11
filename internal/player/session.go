package player

import (
	"context"
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
