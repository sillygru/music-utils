CREATE TABLE IF NOT EXISTS tracks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    name_lower TEXT NOT NULL,
    artist_name TEXT NOT NULL,
    artist_name_lower TEXT NOT NULL,
    album_name TEXT,
    album_name_lower TEXT,
    duration REAL,
    genre TEXT,
    genre_lower TEXT,
    year INTEGER,
    release_date TEXT,
    isrc TEXT,
    musicbrainz_recording_id TEXT,
    musicbrainz_release_id TEXT,
    musicbrainz_release_group_id TEXT,
    musicbrainz_artist_id TEXT,
    cover_url TEXT,
    metadata_source TEXT,
    cover_url_source TEXT,
    metadata_checked BOOLEAN NOT NULL DEFAULT 0,
    cover_url_checked BOOLEAN NOT NULL DEFAULT 0,
    last_lyrics_id INTEGER,
    source TEXT NOT NULL DEFAULT 'local',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(name_lower, artist_name_lower, album_name_lower, duration)
);

CREATE INDEX IF NOT EXISTS idx_tracks_lookup ON tracks(name_lower, artist_name_lower, album_name_lower, duration);
CREATE INDEX IF NOT EXISTS idx_tracks_musicbrainz_recording ON tracks(musicbrainz_recording_id);

CREATE VIRTUAL TABLE IF NOT EXISTS tracks_fts USING fts5(
    name_lower, artist_name_lower, album_name_lower, genre_lower,
    content='tracks', content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS tracks_ai AFTER INSERT ON tracks BEGIN
    INSERT INTO tracks_fts(rowid, name_lower, artist_name_lower, album_name_lower, genre_lower)
    VALUES (new.id, new.name_lower, new.artist_name_lower, new.album_name_lower, new.genre_lower);
END;
CREATE TRIGGER IF NOT EXISTS tracks_ad AFTER DELETE ON tracks BEGIN
    INSERT INTO tracks_fts(tracks_fts, rowid, name_lower, artist_name_lower, album_name_lower, genre_lower)
    VALUES ('delete', old.id, old.name_lower, old.artist_name_lower, old.album_name_lower, old.genre_lower);
END;
CREATE TRIGGER IF NOT EXISTS tracks_au AFTER UPDATE ON tracks BEGIN
    INSERT INTO tracks_fts(tracks_fts, rowid, name_lower, artist_name_lower, album_name_lower, genre_lower)
    VALUES ('delete', old.id, old.name_lower, old.artist_name_lower, old.album_name_lower, old.genre_lower);
    INSERT INTO tracks_fts(rowid, name_lower, artist_name_lower, album_name_lower, genre_lower)
    VALUES (new.id, new.name_lower, new.artist_name_lower, new.album_name_lower, new.genre_lower);
END;
