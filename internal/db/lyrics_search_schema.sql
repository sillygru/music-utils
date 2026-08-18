CREATE TABLE IF NOT EXISTS lyrics_search_cache (
    cache_key TEXT PRIMARY KEY,
    response_json TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_lyrics_search_cache_updated_at ON lyrics_search_cache(updated_at);
