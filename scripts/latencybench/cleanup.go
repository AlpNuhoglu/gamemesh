package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// A player is a bench account only if BOTH conditions hold: the e-mail
// matches a LIKE pattern (.invalid is a reserved TLD, RFC 2606, so no real
// player can have such an address) AND the username starts with "bench". The
// pattern is a LIKE pattern with '\' as the escape character, so "\_" is a
// literal "_".
const (
	emailDomain = "latencybench.invalid"
	// allBenchPattern is the -cleanup-only e-mail pattern.
	allBenchPattern = `%@` + emailDomain
	// usernamePattern is the second, always-applied condition.
	usernamePattern = `bench%`
	// benchPredicate is the one WHERE clause used by the count, the select
	// and the delete, so they cannot drift. $1 is the e-mail pattern.
	benchPredicate = `email LIKE $1 ESCAPE '\' AND username LIKE 'bench%'`
)

// runPattern is the e-mail pattern of one run's accounts only.
func runPattern(runID string) string { return `bench\_` + runID + `\_%@` + emailDomain }

// describePredicate is the human-readable form of benchPredicate.
func describePredicate(pattern string) string {
	return fmt.Sprintf("email LIKE %s AND username LIKE '%s'", pattern, usernamePattern)
}

// cleanupOptions says what to delete and how much.
type cleanupOptions struct {
	DSN     string
	Pattern string   // e-mail pattern
	Limit   int      // max players deleted per call, > 0
	DryRun  bool     // count only
	JTIs    []string // known JWT IDs of this run (SessionRevoked rows)
}

// cleanupResult reports what matched and how many rows each statement deleted.
type cleanupResult struct {
	Pattern        string `json:"pattern"`
	DryRun         bool   `json:"dry_run"`
	Matched        int64  `json:"matched"`
	Limit          int    `json:"limit"`
	Players        int64  `json:"players"`
	PlayerStats    int64  `json:"player_stats"`
	OutboxPlayer   int64  `json:"outbox_events_player"`
	OutboxSessions int64  `json:"outbox_events_session_revoked"`
	Remaining      int64  `json:"remaining"`
	Error          string `json:"error,omitempty"`
}

// deleteBenchAccounts counts the bench players matching the predicate, prints
// the count, and (unless DryRun) deletes up to Limit of them, lowest id first,
// and the rows that reference them, in one transaction:
//
//   - player_stats (FK to players, ON DELETE CASCADE; deleted explicitly so
//     the count can be reported),
//   - outbox_events rows whose payload player_id is one of the selected
//     players (PlayerRegistered, PlayerUpdated), so the delete is bounded by
//     the limit,
//   - outbox_events SessionRevoked rows for the given JWT IDs. Their payload
//     holds only the jti, so they can be matched only when this process knows
//     the tokens (a normal run); -cleanup-only cannot find them.
//
// If more players match than Limit, Remaining says how many are left and the
// command has to be run again.
//
// Nothing else in Postgres references a player (migrations/ and the player
// service's AutoMigrate list only players, player_stats and outbox_events).
func deleteBenchAccounts(ctx context.Context, o cleanupOptions) (cleanupResult, error) {
	res := cleanupResult{Pattern: o.Pattern, DryRun: o.DryRun, Limit: o.Limit}
	if o.Limit <= 0 {
		return res, errors.New("cleanup limit must be > 0")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, o.DSN)
	if err != nil {
		return res, fmt.Errorf("connect to postgres: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // no-op after Commit

	// Dry run first: count, print, and stop here if asked to.
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM players WHERE `+benchPredicate, o.Pattern).Scan(&res.Matched); err != nil {
		return res, fmt.Errorf("count bench players: %w", err)
	}
	log.Printf("cleanup dry run: %d player(s) match (%s)", res.Matched, describePredicate(o.Pattern))
	if o.DryRun {
		res.Remaining = res.Matched
		return res, nil
	}

	rows, err := tx.Query(ctx, `SELECT id::text FROM players WHERE `+benchPredicate+` ORDER BY id LIMIT $2 FOR UPDATE`, o.Pattern, o.Limit)
	if err != nil {
		return res, fmt.Errorf("select bench players: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return res, fmt.Errorf("select bench players: %w", err)
	}

	var hasOutbox bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('outbox_events') IS NOT NULL`).Scan(&hasOutbox); err != nil {
		return res, err
	}
	if hasOutbox {
		tag, err := tx.Exec(ctx, `DELETE FROM outbox_events WHERE payload->>'player_id' = ANY($1::text[])`, ids)
		if err != nil {
			return res, fmt.Errorf("delete outbox_events (player): %w", err)
		}
		res.OutboxPlayer = tag.RowsAffected()
		if len(o.JTIs) > 0 {
			tag, err = tx.Exec(ctx, `
				DELETE FROM outbox_events
				 WHERE event_type = 'SessionRevoked' AND payload->>'jti' = ANY($1::text[])`, o.JTIs)
			if err != nil {
				return res, fmt.Errorf("delete outbox_events (SessionRevoked): %w", err)
			}
			res.OutboxSessions = tag.RowsAffected()
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM player_stats WHERE player_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return res, fmt.Errorf("delete player_stats: %w", err)
	}
	res.PlayerStats = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `DELETE FROM players WHERE id = ANY($2::uuid[]) AND `+benchPredicate, o.Pattern, ids)
	if err != nil {
		return res, fmt.Errorf("delete players: %w", err)
	}
	res.Players = tag.RowsAffected()
	if res.Players != int64(len(ids)) {
		return res, errors.New("player row count changed inside the transaction; rolled back")
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	res.Remaining = res.Matched - res.Players
	if res.Remaining > 0 {
		log.Printf("cleanup: %d player(s) remain beyond -cleanup-limit %d; run the command again", res.Remaining, o.Limit)
	}
	return res, nil
}

func (r cleanupResult) String() string {
	if r.DryRun {
		return fmt.Sprintf("dry run: matched=%d limit=%d, nothing deleted", r.Matched, r.Limit)
	}
	return fmt.Sprintf("matched=%d limit=%d deleted: players=%d player_stats=%d outbox_events(player events)=%d outbox_events(SessionRevoked)=%d; remaining=%d",
		r.Matched, r.Limit, r.Players, r.PlayerStats, r.OutboxPlayer, r.OutboxSessions, r.Remaining)
}
