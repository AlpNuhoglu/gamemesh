-- Guest accounts. A guest is an ordinary player row whose username and
-- password the server generated; these two columns are all that tells it
-- apart and all the sweep needs.
--
-- Additive only, on purpose: the player service's AutoMigrate applies the
-- same change from the GORM model, and adding a column with a default cannot
-- fail on existing rows. e-mail and password_hash stay NOT NULL; a guest row
-- carries a placeholder address ("<id>@guest.invalid") and the hash of its
-- generated secret.

-- 1. Which accounts are guests. Existing accounts are not.
ALTER TABLE players
    ADD COLUMN IF NOT EXISTS is_guest BOOLEAN NOT NULL DEFAULT false;

-- 2. When the account last took a token. NULL for accounts that never have
--    since this column exists; only guests are swept by it.
ALTER TABLE players
    ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ;

-- 3. The sweep's lookup: guests by last activity. The name matches the GORM
--    model's index tag so AutoMigrate recognises it and leaves it alone.
CREATE INDEX IF NOT EXISTS idx_players_guest_last_seen
    ON players (is_guest, last_seen_at);
