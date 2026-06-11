-- Players: core identity. UUIDs are generated client-side by the service,
-- but a DB default is kept as a safety net for manual inserts.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS players (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      VARCHAR(32)  NOT NULL,
    email         VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- Constraint names match GORM's uni_<table>_<column> convention so the
    -- player service's AutoMigrate recognises them and leaves them alone.
    CONSTRAINT uni_players_username UNIQUE (username),
    CONSTRAINT uni_players_email    UNIQUE (email),
    CONSTRAINT ck_players_username_len CHECK (char_length(username) >= 3)
);

-- Login looks up by username OR email; both unique constraints double as
-- the lookup indexes.
