-- Transactional outbox: domain events are inserted here in the SAME Postgres
-- transaction as the business rows (players, player_stats). A separate relay
-- process publishes committed rows to NATS, eliminating the dual-write problem.
CREATE TABLE IF NOT EXISTS outbox_events (
    id            UUID        PRIMARY KEY,              -- = events.Event.ID, reused as the consumer dedup key
    event_type    TEXT        NOT NULL,                 -- e.g. "PlayerRegistered"
    topic         TEXT        NOT NULL,                 -- e.g. "events.player"
    payload       JSONB       NOT NULL,                 -- domain payload (events.Event.Payload)
    carrier       JSONB       NOT NULL DEFAULT '{}',    -- W3C trace headers captured at write time
    status        TEXT        NOT NULL DEFAULT 'PENDING',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at  TIMESTAMPTZ,                          -- NULL until relayed to NATS
    attempt_count INTEGER     NOT NULL DEFAULT 0,
    CONSTRAINT ck_outbox_status CHECK (status IN ('PENDING', 'PUBLISHED'))
);

-- Relay hot path: fetch the oldest unpublished rows. The partial index stays
-- small as PUBLISHED rows accumulate, keeping the poll query O(batch).
CREATE INDEX IF NOT EXISTS idx_outbox_pending
    ON outbox_events (created_at)
    WHERE status = 'PENDING';
