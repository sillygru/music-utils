package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyCombinedDatabase(t *testing.T) {
	ctx := context.Background()
	cfg := Config{MmapSize: 64 * 1024 * 1024, CacheSizeKB: -8000, MaxOpenConns: 1}
	metadataPath := filepath.Join(t.TempDir(), "combined.db")
	metadataDB, err := Open(metadataPath, cfg)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer metadataDB.Close()
	lyricsDB, err := Open(filepath.Join(t.TempDir(), "lyrics.db"), cfg)
	if err != nil {
		t.Fatalf("open lyrics database: %v", err)
	}
	defer lyricsDB.Close()

	if _, err = metadataDB.ExecContext(ctx, `
CREATE TABLE tracks (
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
CREATE TABLE lyrics (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    track_id INTEGER NOT NULL,
    plain_lyrics TEXT,
    synced_lyrics TEXT,
    has_plain_lyrics BOOLEAN NOT NULL DEFAULT 0,
    has_synced_lyrics BOOLEAN NOT NULL DEFAULT 0,
    instrumental BOOLEAN NOT NULL DEFAULT 0,
    content_hash TEXT NOT NULL UNIQUE,
    source TEXT NOT NULL DEFAULT 'local',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (track_id) REFERENCES tracks(id)
);
CREATE TABLE lyrics_tracks (
    track_id INTEGER NOT NULL,
    lyrics_id INTEGER NOT NULL,
    PRIMARY KEY (track_id, lyrics_id),
    FOREIGN KEY (track_id) REFERENCES tracks(id) ON DELETE CASCADE,
    FOREIGN KEY (lyrics_id) REFERENCES lyrics(id) ON DELETE CASCADE
);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err = metadataDB.ExecContext(ctx, `INSERT INTO tracks (name,name_lower,artist_name,artist_name_lower,album_name,album_name_lower,duration) VALUES ('Legacy Song','legacy song','Legacy Artist','legacy artist','Legacy Album','legacy album',180)`); err != nil {
		t.Fatalf("insert legacy track: %v", err)
	}
	if _, err = metadataDB.ExecContext(ctx, `INSERT INTO lyrics (track_id,plain_lyrics,has_plain_lyrics,content_hash,source) VALUES (1,'legacy words',1,'legacy-hash','local')`); err != nil {
		t.Fatalf("insert legacy lyrics: %v", err)
	}
	if _, err = metadataDB.ExecContext(ctx, `INSERT INTO lyrics_tracks (track_id,lyrics_id) VALUES (1,1)`); err != nil {
		t.Fatalf("insert legacy association: %v", err)
	}
	if _, err = metadataDB.ExecContext(ctx, `UPDATE tracks SET last_lyrics_id=1 WHERE id=1`); err != nil {
		t.Fatalf("link legacy lyrics: %v", err)
	}

	if err = MigrateLyrics(ctx, lyricsDB); err != nil {
		t.Fatalf("migrate lyrics database: %v", err)
	}
	if err = MigrateMetadata(ctx, metadataDB); err != nil {
		t.Fatalf("migrate metadata database: %v", err)
	}
	if err = MigrateLegacyLyrics(ctx, metadataDB, lyricsDB); err != nil {
		t.Fatalf("migrate legacy lyrics: %v", err)
	}

	var tableCount int
	if err = metadataDB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('lyrics','lyrics_tracks','tracks_legacy')").Scan(&tableCount); err != nil {
		t.Fatalf("inspect old lyrics tables: %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("legacy lyrics tables remain: %d", tableCount)
	}
	var foreignKeyCount int
	if err = metadataDB.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_list('tracks')").Scan(&foreignKeyCount); err != nil {
		t.Fatalf("inspect metadata foreign keys: %v", err)
	}
	if foreignKeyCount != 0 {
		t.Fatalf("metadata database retained cross-database foreign keys: %d", foreignKeyCount)
	}
	var copiedLyrics string
	if err = lyricsDB.QueryRowContext(ctx, "SELECT plain_lyrics FROM lyrics WHERE id=1").Scan(&copiedLyrics); err != nil {
		t.Fatalf("read copied lyrics: %v", err)
	}
	if copiedLyrics != "legacy words" {
		t.Fatalf("unexpected copied lyrics: %q", copiedLyrics)
	}

	track, lyrics, err := FindTrackExact(ctx, metadataDB, lyricsDB, "Legacy Song", "Legacy Artist", "Legacy Album", 180)
	if err != nil {
		t.Fatalf("find migrated track: %v", err)
	}
	if track.Name != "Legacy Song" || lyrics.PlainLyrics != "legacy words" {
		t.Fatalf("unexpected migrated result: %+v / %+v", track, lyrics)
	}
	if _, err = UpsertTrackMetadata(ctx, metadataDB, Track{Name: "New Song", ArtistName: "New Artist", Duration: 90}); err != nil {
		t.Fatalf("write metadata after migration: %v", err)
	}

	if err = MigrateMetadata(ctx, metadataDB); err != nil {
		t.Fatalf("rerun metadata migration: %v", err)
	}
	if err = MigrateLyrics(ctx, lyricsDB); err != nil {
		t.Fatalf("rerun lyrics migration: %v", err)
	}
	if err = MigrateLegacyLyrics(ctx, metadataDB, lyricsDB); err != nil {
		t.Fatalf("rerun legacy migration: %v", err)
	}
}

var _ *sql.DB
