-- auto_vacuum = INCREMENTAL is set by db.Open before WAL is enabled; it
-- cannot take effect here because the WAL switch already wrote the header.

-- Key-value settings edited on the Settings page: retention days, daily API
-- budget, default profile. Missing keys fall back to defaults in code.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
