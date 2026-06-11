-- GameMesh full schema (concatenation of all up-migrations).
-- Mounted into the PostgreSQL container's docker-entrypoint-initdb.d so a
-- fresh `docker compose up` starts with the schema in place.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS players (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      VARCHAR(32)  NOT NULL,
    email         VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT uni_players_username UNIQUE (username),
    CONSTRAINT uni_players_email    UNIQUE (email),
    CONSTRAINT ck_players_username_len CHECK (char_length(username) >= 3)
);

CREATE TABLE IF NOT EXISTS player_stats (
    player_id    UUID PRIMARY KEY REFERENCES players (id) ON DELETE CASCADE,
    rank         INTEGER     NOT NULL DEFAULT 1000,
    score        BIGINT      NOT NULL DEFAULT 0,
    games_played INTEGER     NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_player_stats_rank  CHECK (rank >= 0),
    CONSTRAINT ck_player_stats_games CHECK (games_played >= 0)
);

CREATE INDEX IF NOT EXISTS idx_player_stats_rank ON player_stats (rank);
