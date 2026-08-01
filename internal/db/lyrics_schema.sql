CREATE TABLE IF NOT EXISTS lyrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    track_id INTEGER NOT NULL,
    plain_lyrics TEXT,
    synced_lyrics TEXT,
    has_plain_lyrics BOOLEAN NOT NULL DEFAULT 0,
    has_synced_lyrics BOOLEAN NOT NULL DEFAULT 0,
    instrumental BOOLEAN NOT NULL DEFAULT 0,
    content_hash TEXT NOT NULL UNIQUE,
    source TEXT NOT NULL DEFAULT 'local',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS lyrics_tracks (
    track_id INTEGER NOT NULL,
    lyrics_id INTEGER NOT NULL,
    PRIMARY KEY (track_id, lyrics_id)
);
CREATE INDEX IF NOT EXISTS idx_lyrics_tracks_lyrics_id ON lyrics_tracks(lyrics_id);
CREATE INDEX IF NOT EXISTS idx_lyrics_tracks_track_id ON lyrics_tracks(track_id);
CREATE INDEX IF NOT EXISTS idx_lyrics_content_hash ON lyrics(content_hash);
