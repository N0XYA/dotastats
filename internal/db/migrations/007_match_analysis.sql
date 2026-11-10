-- Match analysis (the analysis package), computed at extraction time.

-- Facts for the timeline and map: deaths, buildings, Roshan, aegis,
-- couriers, buybacks, teamfights, chat. Dropped with parsed detail by
-- detail retention.
CREATE TABLE match_events (
    match_id    INTEGER NOT NULL REFERENCES matches (match_id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,   -- time order
    time        INTEGER NOT NULL,   -- seconds from the horn
    kind        TEXT    NOT NULL,
    player_slot INTEGER,            -- actor
    target_slot INTEGER,            -- victim of a death
    key         TEXT,               -- unit, building or chat text
    value       INTEGER,
    data        TEXT,               -- JSON
    PRIMARY KEY (match_id, seq)
) STRICT;

-- Findings per match or player. Evidence holds numbers, never prose; the
-- pages render text from templates. Kept when raw JSON and detail are
-- pruned, and rebuilt by reextract.
CREATE TABLE match_insights (
    match_id         INTEGER NOT NULL REFERENCES matches (match_id) ON DELETE CASCADE,
    seq              INTEGER NOT NULL,
    player_slot      INTEGER,       -- NULL for the match or a team
    kind             TEXT    NOT NULL,
    time             INTEGER,
    severity         TEXT    NOT NULL CHECK (severity IN ('good', 'bad', 'info')),
    evidence         TEXT    NOT NULL,
    detector_version INTEGER NOT NULL,
    PRIMARY KEY (match_id, seq)
) STRICT;
CREATE INDEX match_insights_kind ON match_insights (kind);

-- Chat wheel phrases from /constants/chat_wheel. Some messages are
-- localization keys; the name is the fallback.
CREATE TABLE chat_wheel (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL,
    message TEXT NOT NULL
) STRICT;

-- Fetch constants again to fill chat_wheel.
DELETE FROM collector_state WHERE key = 'constants_synced_at';
