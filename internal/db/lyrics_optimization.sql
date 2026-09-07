-- Additive performance indexes for rich sync lookups.
CREATE INDEX IF NOT EXISTS idx_lyrics_sync_variants_track_sync ON lyrics_sync_variants(track_id, sync_type);
CREATE INDEX IF NOT EXISTS idx_lyrics_sync_variants_track_sync_source ON lyrics_sync_variants(track_id, sync_type, source);
