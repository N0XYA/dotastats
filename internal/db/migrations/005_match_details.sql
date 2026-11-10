-- Match details, replay parsing and aggregates. Every table below except
-- match_raw, parse_jobs and the aggregates is rebuilt from match_raw by
-- reextract.

-- When the full match was last stored; NULL until details are fetched.
ALTER TABLE matches ADD COLUMN details_at INTEGER;

-- GET /matches/{id} as returned, zstd-compressed. Deleted by retention;
-- matches.raw_pruned_at records when.
CREATE TABLE match_raw (
    match_id       INTEGER PRIMARY KEY REFERENCES matches (match_id) ON DELETE CASCADE,
    fetched_at     INTEGER NOT NULL,
    parser_version INTEGER,         -- OpenDota "version"; NULL when unparsed
    data           BLOB NOT NULL
) STRICT;

-- Team advantage per minute, for the net worth and XP graphs. JSON arrays.
CREATE TABLE match_timeseries (
    match_id         INTEGER PRIMARY KEY REFERENCES matches (match_id) ON DELETE CASCADE,
    radiant_gold_adv TEXT,
    radiant_xp_adv   TEXT
) STRICT;

-- Parsed matches only: per-minute values as JSON arrays.
CREATE TABLE player_timeseries (
    match_id    INTEGER NOT NULL REFERENCES matches (match_id) ON DELETE CASCADE,
    player_slot INTEGER NOT NULL,
    gold_t      TEXT,
    xp_t        TEXT,
    lh_t        TEXT,
    dn_t        TEXT,
    PRIMARY KEY (match_id, player_slot)
) STRICT;

-- Parsed matches only. seq is the position in purchase_log: the same item
-- can be bought twice in the same second.
CREATE TABLE item_purchases (
    match_id    INTEGER NOT NULL REFERENCES matches (match_id) ON DELETE CASCADE,
    player_slot INTEGER NOT NULL,
    seq         INTEGER NOT NULL,
    time        INTEGER NOT NULL,   -- seconds from the horn; negative before it
    item        TEXT    NOT NULL,   -- items.name
    PRIMARY KEY (match_id, player_slot, seq)
) STRICT;

-- Parsed matches only. seq is the position in the placing player's log.
CREATE TABLE wards (
    match_id    INTEGER NOT NULL REFERENCES matches (match_id) ON DELETE CASCADE,
    player_slot INTEGER NOT NULL,
    type        TEXT    NOT NULL CHECK (type IN ('obs', 'sen')),
    seq         INTEGER NOT NULL,
    time        INTEGER NOT NULL,
    x           REAL    NOT NULL,
    y           REAL    NOT NULL,
    left_time   INTEGER,            -- destroyed or expired; NULL if not logged
    PRIMARY KEY (match_id, player_slot, type, seq)
) STRICT;

-- Per-player benchmark percentiles as OpenDota computes them.
CREATE TABLE player_benchmarks (
    match_id    INTEGER NOT NULL REFERENCES matches (match_id) ON DELETE CASCADE,
    player_slot INTEGER NOT NULL,
    metric      TEXT    NOT NULL,   -- gold_per_min, xp_per_min, ...
    raw         REAL,
    pct         REAL,               -- 0..1
    PRIMARY KEY (match_id, player_slot, metric)
) STRICT;

CREATE TABLE parse_jobs (
    match_id      INTEGER PRIMARY KEY REFERENCES matches (match_id) ON DELETE CASCADE,
    job_id        INTEGER,
    requested_at  INTEGER NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,   -- polls so far
    next_check_at INTEGER,
    -- First poll that found the job gone from OpenDota's queue while the
    -- match was still unparsed; a second such poll marks it expired.
    left_queue_at INTEGER,
    last_error    TEXT,
    status        TEXT    NOT NULL CHECK (status IN ('pending', 'done', 'expired', 'failed'))
) STRICT;
CREATE INDEX parse_jobs_status ON parse_jobs (status, next_check_at);

-- Daily /heroStats snapshot: the pick and win counts, keys as returned
-- (1_pick .. 8_win per rank bracket, pub_*, turbo_*, pro_*).
CREATE TABLE hero_stats_cache (
    hero_id    INTEGER NOT NULL,
    fetched_on TEXT    NOT NULL,   -- YYYY-MM-DD, UTC
    data       TEXT    NOT NULL,
    PRIMARY KEY (hero_id, fetched_on)
) STRICT;

-- Latest /benchmarks?hero_id= result per hero.
CREATE TABLE hero_benchmarks (
    hero_id    INTEGER PRIMARY KEY,
    fetched_at INTEGER NOT NULL,
    data       TEXT    NOT NULL
) STRICT;
