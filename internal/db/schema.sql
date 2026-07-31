CREATE TABLE IF NOT EXISTS tracks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    name_lower TEXT NOT NULL,
    artist_name TEXT NOT NULL,
    artist_name_lower TEXT NOT NULL,
    album_name TEXT,
    album_name_lower TEXT,
    duration REAL,
    last_lyrics_id INTEGER,
    source TEXT NOT NULL DEFAULT 'local',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (last_lyrics_id) REFERENCES lyrics(id),
    UNIQUE(name_lower, artist_name_lower, album_name_lower, duration)
);

CREATE TABLE IF NOT EXISTS lyrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    track_id INTEGER NOT NULL,
    plain_lyrics TEXT,
    synced_lyrics TEXT,
    has_plain_lyrics BOOLEAN NOT NULL DEFAULT 0,
    has_synced_lyrics BOOLEAN NOT NULL DEFAULT 0,
    instrumental BOOLEAN NOT NULL DEFAULT 0,
    content_hash TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'local',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (track_id) REFERENCES tracks(id),
    UNIQUE(content_hash)
);

-- A lyrics body can be shared by multiple tracks after content deduplication.
-- Keep the explicit association so every track-to-lyrics relationship remains
-- queryable even though lyrics.track_id records the original owning track.
CREATE TABLE IF NOT EXISTS lyrics_tracks (
    track_id INTEGER NOT NULL,
    lyrics_id INTEGER NOT NULL,
    PRIMARY KEY (track_id, lyrics_id),
    FOREIGN KEY (track_id) REFERENCES tracks(id) ON DELETE CASCADE,
    FOREIGN KEY (lyrics_id) REFERENCES lyrics(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_lyrics_tracks_lyrics_id ON lyrics_tracks(lyrics_id);

CREATE INDEX IF NOT EXISTS idx_tracks_lookup ON tracks(name_lower, artist_name_lower, duration);

CREATE VIRTUAL TABLE IF NOT EXISTS tracks_fts USING fts5(
    name_lower,
    artist_name_lower,
    album_name_lower,
    content='tracks',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS tracks_ai AFTER INSERT ON tracks BEGIN
    INSERT INTO tracks_fts(rowid, name_lower, artist_name_lower, album_name_lower)
    VALUES (new.id, new.name_lower, new.artist_name_lower, new.album_name_lower);
END;

CREATE TRIGGER IF NOT EXISTS tracks_ad AFTER DELETE ON tracks BEGIN
    INSERT INTO tracks_fts(tracks_fts, rowid, name_lower, artist_name_lower, album_name_lower)
    VALUES ('delete', old.id, old.name_lower, old.artist_name_lower, old.album_name_lower);
END;

CREATE TRIGGER IF NOT EXISTS tracks_au AFTER UPDATE ON tracks BEGIN
    INSERT INTO tracks_fts(tracks_fts, rowid, name_lower, artist_name_lower, album_name_lower)
    VALUES ('delete', old.id, old.name_lower, old.artist_name_lower, old.album_name_lower);
    INSERT INTO tracks_fts(rowid, name_lower, artist_name_lower, album_name_lower)
    VALUES (new.id, new.name_lower, new.artist_name_lower, new.album_name_lower);
END;
