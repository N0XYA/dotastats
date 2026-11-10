-- Split daily API usage by account for the Status page. account_id 0 holds
-- calls not made for one account (constants, aggregates).
CREATE TABLE api_usage_new (
    day        TEXT    NOT NULL,    -- YYYY-MM-DD, UTC
    account_id INTEGER NOT NULL,
    calls      INTEGER NOT NULL,
    PRIMARY KEY (day, account_id)
) STRICT;

INSERT INTO api_usage_new (day, account_id, calls)
SELECT day, 0, calls FROM api_usage;

DROP TABLE api_usage;
ALTER TABLE api_usage_new RENAME TO api_usage;
