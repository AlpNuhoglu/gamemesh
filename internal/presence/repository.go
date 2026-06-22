package presence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix namespaces every presence key. A player's record lives at
// presence:{playerID}.
const keyPrefix = "presence:"

func key(playerID string) string { return keyPrefix + playerID }

// ErrInvalidTransition is returned by SetState when the requested transition is
// rejected by the (permissive) state machine.
var ErrInvalidTransition = errors.New("invalid presence transition")

// Repository is the Redis-backed presence store. All mutating operations are
// implemented as Lua scripts so a read-modify-write (e.g. incrementing the
// connection count and possibly changing state) is atomic on the server: two WS
// replicas connecting/disconnecting the same player concurrently can never lose
// an update or clobber each other.
type Repository struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewRepository wraps an existing Redis client. ttl is the presence key
// lifetime; heartbeats refresh it, and once it lapses the player is OFFLINE.
func NewRepository(rdb *redis.Client, ttl time.Duration) *Repository {
	return &Repository{rdb: rdb, ttl: ttl}
}

// mutation is the JSON result every mutating Lua script returns: the record as
// it was before the call (or an OFFLINE zero value if the key was absent) and
// as it is after. The service uses both to decide which event to emit.
type mutation struct {
	Previous Record
	Current  Record
}

// --- Lua scripts -----------------------------------------------------------
//
// Each script reads the current record (cjson), mutates it, writes it back with
// the TTL via SET ... EX, and returns {prev, cur} as a JSON array so Go can
// unmarshal both halves in one round trip. Absent keys decode to an OFFLINE
// zero value. A connection_count that reaches 0 deletes the key entirely so the
// player reverts to "missing == OFFLINE" with no lingering record.

const luaPreamble = `
local raw = redis.call('GET', KEYS[1])
local prev
if raw then
  prev = cjson.decode(raw)
else
  prev = {state='OFFLINE', last_seen=0, connection_count=0, updated_at=0}
end
local cur = {state=prev.state, last_seen=prev.last_seen, connection_count=prev.connection_count, updated_at=prev.updated_at}
local nowSec = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
`

// connect: bump connection_count; if the player was OFFLINE (no live session)
// promote to ONLINE. An already-elevated state (IN_QUEUE/IN_MATCH/AWAY) is
// preserved so opening a second device does not knock a player out of queue.
const luaConnect = luaPreamble + `
cur.connection_count = cur.connection_count + 1
if cur.state == 'OFFLINE' then
  cur.state = 'ONLINE'
end
cur.last_seen = nowSec
cur.updated_at = nowSec
redis.call('SET', KEYS[1], cjson.encode(cur), 'EX', ttl)
return cjson.encode({prev, cur})
`

// disconnect: drop connection_count; at 0 the player is fully gone — delete the
// key so it reads as OFFLINE. Never goes negative.
const luaDisconnect = luaPreamble + `
cur.connection_count = cur.connection_count - 1
if cur.connection_count <= 0 then
  cur.connection_count = 0
  cur.state = 'OFFLINE'
  cur.last_seen = nowSec
  cur.updated_at = nowSec
  redis.call('DEL', KEYS[1])
  return cjson.encode({prev, cur})
end
cur.last_seen = nowSec
cur.updated_at = nowSec
redis.call('SET', KEYS[1], cjson.encode(cur), 'EX', ttl)
return cjson.encode({prev, cur})
`

// heartbeat: refresh last_seen/updated_at and the TTL. If the key is absent
// (expired between beats, or first beat for a session WS already owns) treat it
// as an implicit connect so presence self-heals without an explicit connect.
const luaHeartbeat = luaPreamble + `
if not raw then
  cur.connection_count = 1
  cur.state = 'ONLINE'
end
cur.last_seen = nowSec
cur.updated_at = nowSec
redis.call('SET', KEYS[1], cjson.encode(cur), 'EX', ttl)
return cjson.encode({prev, cur})
`

// setState: write the requested state (ARGV[3]) and refresh the TTL. The
// permissive transition rule is enforced in Go before this runs; the script
// also refuses to resurrect a missing key into IN_QUEUE/IN_MATCH (those need an
// active connection) so a stale API call cannot fabricate presence.
const luaSetState = luaPreamble + `
local target = ARGV[3]
if not raw and (target == 'IN_QUEUE' or target == 'IN_MATCH') then
  return cjson.encode({prev, prev})
end
if not raw then
  cur.connection_count = 0
end
cur.state = target
cur.updated_at = nowSec
if cur.last_seen == 0 then cur.last_seen = nowSec end
redis.call('SET', KEYS[1], cjson.encode(cur), 'EX', ttl)
return cjson.encode({prev, cur})
`

var (
	scriptConnect    = redis.NewScript(luaConnect)
	scriptDisconnect = redis.NewScript(luaDisconnect)
	scriptHeartbeat  = redis.NewScript(luaHeartbeat)
	scriptSetState   = redis.NewScript(luaSetState)
)

func (r *Repository) ttlSeconds() int { return int(r.ttl / time.Second) }

func (r *Repository) runMutation(ctx context.Context, s *redis.Script, playerID string, extra ...any) (mutation, error) {
	args := append([]any{now().Unix(), r.ttlSeconds()}, extra...)
	res, err := s.Run(ctx, r.rdb, []string{key(playerID)}, args...).Result()
	if err != nil {
		return mutation{}, err
	}
	str, ok := res.(string)
	if !ok {
		return mutation{}, fmt.Errorf("presence: unexpected script result type %T", res)
	}
	var pair [2]Record
	if err := json.Unmarshal([]byte(str), &pair); err != nil {
		return mutation{}, fmt.Errorf("presence: decode script result: %w", err)
	}
	return mutation{Previous: pair[0], Current: pair[1]}, nil
}

// connect registers a new connection for the player (multi-device safe).
// Unexported because it returns the internal mutation plumbing and is only
// driven by Service in this package.
func (r *Repository) connect(ctx context.Context, playerID string) (mutation, error) {
	return r.runMutation(ctx, scriptConnect, playerID)
}

// disconnect drops one connection; the player goes OFFLINE only at zero.
func (r *Repository) disconnect(ctx context.Context, playerID string) (mutation, error) {
	return r.runMutation(ctx, scriptDisconnect, playerID)
}

// heartbeat refreshes the TTL (and re-creates an expired record).
func (r *Repository) heartbeat(ctx context.Context, playerID string) (mutation, error) {
	return r.runMutation(ctx, scriptHeartbeat, playerID)
}

// setState writes an explicit state for the player and refreshes the TTL.
func (r *Repository) setState(ctx context.Context, playerID string, to State) (mutation, error) {
	return r.runMutation(ctx, scriptSetState, playerID, string(to))
}

// HeartbeatMany refreshes the TTL for a batch of players in a single pipelined
// round trip — the bulk fast-path behind the WS replica's heartbeat ticker.
// Instead of one Lua EVAL per player, the common case (the key still exists) is
// one EXPIRE per player, all flushed in ONE Pipeline().
//
// Cluster-ready: uses the non-transactional Pipeline() (not TxPipeline), so a
// go-redis ClusterClient can transparently split the per-key EXPIREs across the
// slots they hash to. Every command is single-key, so there is no cross-slot
// assumption and no hash tag.
//
// Self-heal: EXPIRE on a missing key is a no-op (returns false) — it cannot
// resurrect a record that expired between beats (e.g. after a crash). Those
// misses are collected and re-created as ONLINE in a second, still-pipelined
// pass that runs the same single-key heartbeat Lua script. In steady state the
// second pass is empty, so the hot path stays at one round trip.
//
// Returns the player IDs that were re-created (OFFLINE->ONLINE) so the caller
// can publish PresenceOnline for them; refreshed-but-unchanged players produce
// no event.
func (r *Repository) HeartbeatMany(ctx context.Context, playerIDs []string) ([]string, error) {
	if len(playerIDs) == 0 {
		return nil, nil
	}

	// Pass 1: one EXPIRE per player, all in a single non-transactional pipeline.
	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.BoolCmd, len(playerIDs))
	for i, id := range playerIDs {
		cmds[i] = pipe.Expire(ctx, key(id), r.ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}

	// Collect misses (EXPIRE false → key absent → needs re-create). Zero-length
	// with no capacity: in steady state there are no misses, so this never
	// allocates a backing array on the hot path.
	missed := make([]string, 0)
	for i, id := range playerIDs {
		ok, err := cmds[i].Result()
		if err != nil {
			return nil, err
		}
		if !ok {
			missed = append(missed, id)
		}
	}
	if len(missed) == 0 {
		return nil, nil
	}

	// Pass 2: re-create the expired records as ONLINE. Still pipelined and still
	// single-key Lua EVALs (Cluster-safe); runs only for the rare crash-recovery
	// minority.
	healPipe := r.rdb.Pipeline()
	healCmds := make([]*redis.Cmd, len(missed))
	nowSec := now().Unix()
	ttlSec := r.ttlSeconds()
	for i, id := range missed {
		// Use Eval (full EVAL with source), not Run: Run's EVALSHA->EVAL fallback
		// on NOSCRIPT cannot work inside a pipeline (the error is not seen until
		// Exec), and a fresh Cluster connection may not have the script cached.
		// This pass is the rare crash-recovery minority, so the extra bytes are
		// off the hot path.
		healCmds[i] = scriptHeartbeat.Eval(ctx, healPipe, []string{key(id)}, nowSec, ttlSec)
	}
	if _, err := healPipe.Exec(ctx); err != nil {
		return nil, err
	}
	healed := make([]string, 0, len(missed))
	for i, id := range missed {
		if err := healCmds[i].Err(); err != nil {
			return nil, err
		}
		healed = append(healed, id)
	}
	return healed, nil
}

// Get returns the player's current record. A missing key reads as OFFLINE. This
// is a pure read with no side effects — no events are emitted here.
func (r *Repository) Get(ctx context.Context, playerID string) (Record, error) {
	raw, err := r.rdb.Get(ctx, key(playerID)).Result()
	if errors.Is(err, redis.Nil) {
		return offlineRecord(), nil
	}
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return Record{}, fmt.Errorf("presence: decode record: %w", err)
	}
	return rec, nil
}

// GetMany returns one record per playerID in a single pipelined round trip —
// O(N) GETs, one network round trip, no N+1. Missing keys map to OFFLINE.
func (r *Repository) GetMany(ctx context.Context, playerIDs []string) (map[string]Record, error) {
	out := make(map[string]Record, len(playerIDs))
	if len(playerIDs) == 0 {
		return out, nil
	}
	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(playerIDs))
	for i, id := range playerIDs {
		cmds[i] = pipe.Get(ctx, key(id))
	}
	// Exec returns redis.Nil if ANY command missed; that is expected (offline
	// friends), so we inspect each command rather than failing the batch.
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	for i, id := range playerIDs {
		raw, err := cmds[i].Result()
		if errors.Is(err, redis.Nil) {
			out[id] = offlineRecord()
			continue
		}
		if err != nil {
			return nil, err
		}
		var rec Record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("presence: decode record for %s: %w", id, err)
		}
		out[id] = rec
	}
	return out, nil
}
