-- Revert dead-letter support. Any FAILED rows must be resolved first (re-queued
-- to PENDING or deleted), otherwise the restored two-state check would reject
-- them.
DROP INDEX IF EXISTS idx_outbox_failed;

ALTER TABLE outbox_events DROP COLUMN IF EXISTS last_error;

ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS ck_outbox_status;
ALTER TABLE outbox_events
    ADD CONSTRAINT ck_outbox_status
    CHECK (status IN ('PENDING', 'PUBLISHED'));
