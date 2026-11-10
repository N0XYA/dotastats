-- Tracked accounts: one owner (whoever runs the instance) plus friends.
-- Times are unix seconds.
CREATE TABLE accounts (
    account_id        INTEGER PRIMARY KEY,
    role              TEXT    NOT NULL CHECK (role IN ('owner', 'friend')),
    label             TEXT,
    is_active         INTEGER NOT NULL DEFAULT 1,
    added_at          INTEGER NOT NULL,
    name              TEXT,
    avatar_url        TEXT,
    rank_tier         INTEGER,
    profile_synced_at INTEGER,
    -- When match discovery last succeeded. NULL means the full history has
    -- not been fetched yet, i.e. a backfill is pending.
    last_synced_at    INTEGER
) STRICT;
CREATE UNIQUE INDEX accounts_single_owner ON accounts (role) WHERE role = 'owner';

CREATE TABLE matches (
    match_id      INTEGER PRIMARY KEY,
    start_time    INTEGER NOT NULL,
    duration      INTEGER NOT NULL,
    game_mode     INTEGER NOT NULL,
    lobby_type    INTEGER NOT NULL,
    radiant_win   INTEGER,          -- 1/0, NULL if unknown
    patch         INTEGER,          -- from start_time until details arrive
    region        INTEGER,          -- from details
    avg_rank_tier INTEGER,
    is_parsed     INTEGER NOT NULL DEFAULT 0,
    parse_state   TEXT,
    raw_pruned_at INTEGER
) STRICT;
CREATE INDEX matches_start_time ON matches (start_time);

-- Which tracked accounts played each match. Friends in one party share a
-- single matches row; a match is deleted once its last link is removed.
CREATE TABLE account_matches (
    account_id INTEGER NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    match_id   INTEGER NOT NULL REFERENCES matches (match_id) ON DELETE CASCADE,
    PRIMARY KEY (account_id, match_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX account_matches_match ON account_matches (match_id);

-- Players of a match. Discovery stores each tracked account's own row;
-- match details add the other players and the columns discovery can't get
-- (backpack, net worth, second neutral slot, rank tier, name).
CREATE TABLE match_players (
    match_id      INTEGER NOT NULL REFERENCES matches (match_id) ON DELETE CASCADE,
    player_slot   INTEGER NOT NULL,
    account_id    INTEGER,          -- NULL for anonymous players; never join on it
    hero_id       INTEGER NOT NULL,
    kills         INTEGER NOT NULL,
    deaths        INTEGER NOT NULL,
    assists       INTEGER NOT NULL,
    leaver_status INTEGER,
    party_size    INTEGER,
    personaname   TEXT,
    last_hits     INTEGER,
    denies        INTEGER,
    gold_per_min  INTEGER,
    xp_per_min    INTEGER,
    net_worth     INTEGER,
    hero_damage   INTEGER,
    tower_damage  INTEGER,
    hero_healing  INTEGER,
    level         INTEGER,
    item_0        INTEGER,
    item_1        INTEGER,
    item_2        INTEGER,
    item_3        INTEGER,
    item_4        INTEGER,
    item_5        INTEGER,
    backpack_0    INTEGER,
    backpack_1    INTEGER,
    backpack_2    INTEGER,
    item_neutral  INTEGER,
    item_neutral2 INTEGER,          -- neutral enhancement
    lane          INTEGER,          -- parsed matches only
    lane_role     INTEGER,          -- parsed matches only
    is_roaming    INTEGER,          -- parsed matches only
    rank_tier     INTEGER,
    PRIMARY KEY (match_id, player_slot)
) STRICT;
CREATE INDEX match_players_account_hero ON match_players (account_id, hero_id);

-- Constants from /constants/*. *_src is the image path on the Steam CDN;
-- *_path is the downloaded copy relative to <data-dir>/assets, with forward
-- slashes, NULL until downloaded.
CREATE TABLE heroes (
    id             INTEGER PRIMARY KEY,
    name           TEXT NOT NULL,   -- npc_dota_hero_antimage
    localized_name TEXT NOT NULL,   -- Anti-Mage
    img_src        TEXT,
    icon_src       TEXT,
    img_path       TEXT,
    icon_path      TEXT
) STRICT;

CREATE TABLE items (
    id       INTEGER PRIMARY KEY,
    name     TEXT NOT NULL,         -- blink
    dname    TEXT,                  -- Blink Dagger; missing for a few items
    img_src  TEXT,
    img_path TEXT
) STRICT;

CREATE TABLE game_modes (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL              -- game_mode_all_pick
) STRICT;

CREATE TABLE lobby_types (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL              -- lobby_type_ranked
) STRICT;

CREATE TABLE patches (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,      -- 7.41
    released_at INTEGER NOT NULL
) STRICT;
CREATE INDEX patches_released_at ON patches (released_at);

-- Collector bookkeeping, e.g. when constants were last fetched.
CREATE TABLE collector_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
