-- Per-provider fetch history for lyrics enrichment.
-- Negative (miss) rows are remembered for 24h to avoid re-hitting upstream
-- for a track+provider that was just tried. Positive rows are implicit via
-- lyrics/lyrics_sync_variants, but we also record last attempt here for
-- single-point staleness checks.
CREATE TABLE IF NOT EXISTS lyrics_provider_fetches (
    track_id INTEGER NOT NULL,
    provider TEXT NOT NULL,
    last_fetched_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_success BOOLEAN NOT NULL DEFAULT 0,
    PRIMARY KEY (track_id, provider)
);
CREATE INDEX IF NOT EXISTS idx_provider_fetches_fetched_at ON lyrics_provider_fetches(last_fetched_at);
