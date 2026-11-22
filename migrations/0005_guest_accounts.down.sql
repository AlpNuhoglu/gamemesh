-- Revert guest accounts. Guest rows must be deleted first: without is_guest
-- they would be indistinguishable from registered players.
DROP INDEX IF EXISTS idx_players_guest_last_seen;

ALTER TABLE players DROP COLUMN IF EXISTS last_seen_at;
ALTER TABLE players DROP COLUMN IF EXISTS is_guest;
