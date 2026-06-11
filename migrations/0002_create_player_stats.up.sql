-- Player stats: gameplay-derived state split from identity so frequent
-- rank/score writes never contend with the players row.
CREATE TABLE IF NOT EXISTS player_stats (
    player_id    UUID PRIMARY KEY REFERENCES players (id) ON DELETE CASCADE,
    rank         INTEGER     NOT NULL DEFAULT 1000,
    score        BIGINT      NOT NULL DEFAULT 0,
    games_played INTEGER     NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_player_stats_rank  CHECK (rank >= 0),
    CONSTRAINT ck_player_stats_games CHECK (games_played >= 0)
);

-- Rank-range scans (e.g. backfilling the matchmaking queue, analytics).
CREATE INDEX IF NOT EXISTS idx_player_stats_rank ON player_stats (rank);
