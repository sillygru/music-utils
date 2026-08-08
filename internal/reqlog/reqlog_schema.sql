-- Request access log: one row per HTTP request, append-only. Rowid order
-- matches insert order and the table intentionally carries NO secondary
-- indexes, so it stays a dense stream of pages: small file, fast inserts.
-- Repeated TEXT values (method, endpoint, outcome) live in tiny dictionary
-- tables referenced by INTEGER id, and timestamps are stored as unix epoch
-- milliseconds, so a typical row costs well under 100 bytes.

CREATE TABLE IF NOT EXISTS methods (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS endpoints (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS outcomes (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS request_log (
    id          INTEGER PRIMARY KEY,
    ts          INTEGER NOT NULL,               -- unix epoch milliseconds
    method_id   INTEGER NOT NULL,
    endpoint_id INTEGER NOT NULL,
    status      INTEGER NOT NULL,               -- HTTP status code
    outcome_id  INTEGER NOT NULL,               -- local_hit, miss, rate_limited, ...
    cache_ms    INTEGER NOT NULL DEFAULT 0,     -- time in the local cache lookup
    upstream_ms INTEGER NOT NULL DEFAULT 0,     -- time talking to upstream providers
    params      TEXT NOT NULL DEFAULT ''        -- raw query string, truncated
);
