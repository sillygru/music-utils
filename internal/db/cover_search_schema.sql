-- Whole-response cache for /api/cover/search, mirroring lyrics_search_cache.
--
-- The cover resolver memoizes searches in process, but that map is lost on
-- restart and is not shared with the background jobs. Without a durable copy
-- every repeat of a popular artist or album query pays the provider round trip
-- again, which is what turns a burst of identical searches into a queue behind
-- the shared upstream pacers.
--
-- The cached value is the marshalled response for one canonical query, so a
-- hit never re-derives results. It is deliberately separate from cover_urls:
-- that table stores one resolved winner per entity, while a search can return
-- many candidates across several providers for one free-text term.
CREATE TABLE IF NOT EXISTS cover_search_cache (
    cache_key TEXT PRIMARY KEY,
    response_json TEXT NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_cover_search_cache_updated_at ON cover_search_cache(updated_at);