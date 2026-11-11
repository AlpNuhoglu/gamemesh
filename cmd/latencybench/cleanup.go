package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Bench accounts are recognised by their e-mail alone: .invalid is a reserved
// TLD (RFC 2606), so no real player can have such an address. The pattern is
// a LIKE pattern with '\' as the escape character, so "\_" is a literal "_".
const (
	emailDomain     = "latencybench.invalid"
	allBenchPattern = `bench\_%@` + emailDomain
)

// runPattern matches only the accounts of one run.
func runPattern(runID string) string { return `bench\_` + runID + `\_%@` + emailDomain }

// cleanupResult reports how many rows each statement deleted.
type cleanupResult struct {
	Pattern        string `json:"pattern"`
	Players        int64  `json:"players"`
	PlayerStats    int64  `json:"player_stats"`
	OutboxPlayer   int64  `json:"outbox_events_player"`
	OutboxSessions int64  `json:"outbox_events_session_revoked"`
	Error          string `json:"error,omitempty"`
}

// deleteBenchAccounts removes every player whose e-mail matches pattern, and
// the rows that reference them, in one transaction:
//
//   - player_stats (FK to players, ON DELETE CASCADE; deleted explicitly so
//     the count can be reported),
//   - outbox_events rows whose payload names one of those players by
//     player_id or by a matching e-mail (PlayerRegistered, PlayerUpdated),
//   - outbox_events SessionRevoked rows for the given JWT IDs. Their payload
//     holds only the jti, so they can be matched only when this process knows
//     the tokens (a normal run); -cleanup-only cannot find them.
//
// Nothing else in Postgres references a player (migrations/ and the player
// service's AutoMigrate list only players, player_stats and outbox_events).
func deleteBenchAccounts(ctx context.Context, dsn, pattern string, jtis []string) (cleanupResult, error) {
	res := cleanupResult{Pattern: pattern}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return res, fmt.Errorf("connect to postgres: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // no-op after Commit

	var ids []string
	rows, err := tx.Query(ctx, `SELECT id::text FROM players WHERE email LIKE $1 ESCAPE '\' FOR UPDATE`, pattern)
	if err != nil {
		return res, fmt.Errorf("select bench players: %w", err)
	}
	ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return res, fmt.Errorf("select bench players: %w", err)
	}

	var hasOutbox bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('outbox_events') IS NOT NULL`).Scan(&hasOutbox); err != nil {
		return res, err
	}
	if hasOutbox {
		tag, err := tx.Exec(ctx, `
			DELETE FROM outbox_events
			 WHERE payload->>'player_id' = ANY($1::text[])
			    OR payload->>'email' LIKE $2 ESCAPE '\'`, ids, pattern)
		if err != nil {
			return res, fmt.Errorf("delete outbox_events (player): %w", err)
		}
		res.OutboxPlayer = tag.RowsAffected()
		if len(jtis) > 0 {
			tag, err = tx.Exec(ctx, `
				DELETE FROM outbox_events
				 WHERE event_type = 'SessionRevoked' AND payload->>'jti' = ANY($1::text[])`, jtis)
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
	tag, err = tx.Exec(ctx, `DELETE FROM players WHERE id = ANY($1::uuid[]) AND email LIKE $2 ESCAPE '\'`, ids, pattern)
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
	return res, nil
}

func (r cleanupResult) String() string {
	return fmt.Sprintf("players=%d player_stats=%d outbox_events(player events)=%d outbox_events(SessionRevoked)=%d",
		r.Players, r.PlayerStats, r.OutboxPlayer, r.OutboxSessions)
}
