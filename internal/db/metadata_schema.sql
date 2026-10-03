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
    -- metadata_checked_at is restamped every time a provider gives a definitive
    -- answer, whether that answer is a match or a miss. It answers "when did a
    -- provider last settle this track", so it is NULL when no answer has ever
    -- been given.
    --
    -- NULL is not missing data to be tidied up later. A track checked before
    -- this column existed keeps a NULL forever, and that is the useful reading:
    -- the backfill that could have filled it in would have had to invent a time,
    -- which would make every miss in the existing library look freshly checked
    -- and leave an aging reader with nothing to retry. A NULL says "settled
    -- before we started recording when", which is a category the reader wants
    -- to revisit, not a gap to paper over.
    metadata_checked_at DATETIME,
    -- lyrics_checked is the lyrics counterpart of metadata_checked: it records
    -- that a lyrics provider gave a definitive answer, which is either lyrics or
    -- a confirmed miss. It drives the lyrics backfill's selection.
    --
    -- It lives on tracks rather than in the lyrics database because the two are
    -- separate files and cannot be joined. The selection query lives here, and
    -- the one extra fact it needs from the lyrics side (does this track already
    -- have usable lyrics) is checked per page instead of in SQL.
    lyrics_checked BOOLEAN NOT NULL DEFAULT 0,
    -- lyrics_checked_at follows metadata_checked_at exactly, including the
    -- meaning of NULL: "settled before we started recording when". The lyrics
    -- backfill stamps that onto a track that already had lyrics cached before
    -- these columns existed, so the first run does not re-fetch a library that
    -- is already complete while a later -refresh still sweeps up every row.
    lyrics_checked_at DATETIME,
    cover_url_checked BOOLEAN NOT NULL DEFAULT 0,
    last_lyrics_id INTEGER,
    source TEXT NOT NULL DEFAULT 'local',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(name_lower, artist_name_lower, album_name_lower, duration)
);

CREATE INDEX IF NOT EXISTS idx_tracks_lookup ON tracks(name_lower, artist_name_lower, album_name_lower, duration);
CREATE INDEX IF NOT EXISTS idx_tracks_musicbrainz_recording ON tracks(musicbrainz_recording_id);

-- Serves the backfill's paging query, which asks for unchecked tracks with an id
-- above a cursor. Without it SQLite range-scans the primary key and discards
-- every checked row it passes, so page N costs O(rows remaining) and the job gets
-- slower the more of the library it has already resolved. Over a partial
-- predicate, so the index only holds the pending tracks and shrinks as the job
-- succeeds.
--
-- An aging reader that also wants misses older than some cutoff should not reach
-- for metadata_checked_at with an OR: SQLite cannot serve that shape from a
-- partial index and falls back to a full scan. Union the two lookups instead.
CREATE INDEX IF NOT EXISTS idx_tracks_metadata_pending ON tracks(id) WHERE metadata_checked = 0;

-- Serves the refresh phase of the backfill, which asks for tracks a provider
-- settled before a cutoff, oldest answer first. It covers the age column and the
-- row id together because the scan pages on that pair: without the id the index
-- could not resume mid-scan, and paging on age alone would repeat rows that tie
-- on age.
--
-- Indexed on COALESCE(metadata_checked_at,'') rather than the bare column so the
-- row with an unknown settle time is served from the index instead of forcing
-- the OR mentioned above into a full scan. An empty string sorts before every
-- timestamp, which gives those rows the oldest age and puts them first.
--
-- Not partial: the expression has to be evaluated for every row to be comparable,
-- so filtering to metadata_checked = 1 would only make the lookup re-check the
-- predicate rather than shrink the index enough to matter.
CREATE INDEX IF NOT EXISTS idx_tracks_metadata_age ON tracks(COALESCE(metadata_checked_at,''), id);

-- The lyrics backfill's two phases, mirroring the pair above. Both exist for the
-- same reasons: the partial index shrinks as the job succeeds, and the age index
-- keeps the refresh scan off a full table scan while serving the never-attempted
-- rows (age unknown) first.
CREATE INDEX IF NOT EXISTS idx_tracks_lyrics_pending ON tracks(id) WHERE lyrics_checked = 0;
CREATE INDEX IF NOT EXISTS idx_tracks_lyrics_age ON tracks(COALESCE(lyrics_checked_at,''), id);

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
