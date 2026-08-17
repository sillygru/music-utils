CREATE TABLE IF NOT EXISTS lyrics_sync_variants (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    track_id INTEGER NOT NULL,
    content TEXT NOT NULL,
    format TEXT NOT NULL,
    sync_type TEXT NOT NULL,
    source TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(track_id, format, sync_type, source)
);
CREATE INDEX IF NOT EXISTS idx_lyrics_sync_variants_track_id ON lyrics_sync_variants(track_id);
CREATE INDEX IF NOT EXISTS idx_lyrics_sync_variants_content_hash ON lyrics_sync_variants(content_hash);
