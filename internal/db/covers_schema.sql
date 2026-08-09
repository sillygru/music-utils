-- Album and artist cover URL cache. A row with a NULL cover_url and a set
-- checked_at records a negative result (the sources were consulted and found
-- nothing) so repeat lookups stop spending upstream budget until checked_at
-- expires.
CREATE TABLE IF NOT EXISTS cover_urls (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type TEXT NOT NULL,
    artist_name_lower TEXT NOT NULL,
    album_name_lower TEXT,
    cover_url TEXT,
    cover_source TEXT,
    checked_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(entity_type, artist_name_lower, album_name_lower)
);

-- Every plausible provider cover URL for an album or artist, kept in rank
-- order. Rank 0 is the winner mirrored on the parent cover_urls row, so a
-- cached row can serve alternates (or promote a live one) without re-resolving
-- upstream. Negative rows have no variants.
CREATE TABLE IF NOT EXISTS cover_url_variants (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    cover_url_id INTEGER NOT NULL REFERENCES cover_urls(id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    source TEXT,
    rank INTEGER NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(cover_url_id, url)
);

CREATE INDEX IF NOT EXISTS idx_cover_urls_entity ON cover_urls(entity_type, updated_at);
CREATE INDEX IF NOT EXISTS idx_cover_url_variants_cover ON cover_url_variants(cover_url_id, rank);