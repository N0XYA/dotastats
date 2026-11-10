-- OpenDota calls spent per UTC day. Persisted so the daily budget holds
-- across restarts and is shared by the server and CLI commands.
CREATE TABLE api_usage (
    day   TEXT PRIMARY KEY, -- YYYY-MM-DD, UTC
    calls INTEGER NOT NULL
) STRICT;
