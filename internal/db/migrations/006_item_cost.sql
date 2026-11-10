-- Item cost from /constants/items, used to tell core items from
-- consumables. Forget the last constants fetch so the next run fills it.
ALTER TABLE items ADD COLUMN cost INTEGER;
DELETE FROM collector_state WHERE key = 'constants_synced_at';
