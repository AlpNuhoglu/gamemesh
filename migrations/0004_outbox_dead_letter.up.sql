-- Dead-letter support for the transactional outbox. A row that keeps failing to
-- publish past OUTBOX_MAX_ATTEMPTS is moved to the FAILED state instead of being
-- retried forever, so a single poison row can never wedge a relay worker. FAILED
-- rows drop out of the PENDING poll and are surfaced for operator inspection.

-- 1. Allow the new FAILED status. Drop the two-state check and re-add a
--    three-state one (PENDING is the only status that is ever polled).
ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS ck_outbox_status;
ALTER TABLE outbox_events
    ADD CONSTRAINT ck_outbox_status
    CHECK (status IN ('PENDING', 'PUBLISHED', 'FAILED'));

-- 2. Record why a dead-lettered row never published, for operator triage.
ALTER TABLE outbox_events
    ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '';

-- 3. Small index over the dead-letter queue so an operator (or a replay job) can
--    list FAILED rows cheaply without scanning the published history.
CREATE INDEX IF NOT EXISTS idx_outbox_failed
    ON outbox_events (created_at)
    WHERE status = 'FAILED';
