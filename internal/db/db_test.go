package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenMemoryDatabaseAndPragmas(t *testing.T) {
	database, err := Open(":memory:", Config{
		MmapSize:     512 * 1024 * 1024,
		CacheSizeKB:  -64000,
		MaxOpenConns: 1,
	})
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	defer database.Close()

	checks := map[string]string{
		"journal_mode": "memory",
		"synchronous":  "1",
		"temp_store":   "2",
		"foreign_keys": "1",
		"cache_size":   "-64000",
	}
	for pragma, expected := range checks {
		var actual string
		if err := database.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&actual); err != nil {
			t.Fatalf("read %s pragma: %v", pragma, err)
		}
		if actual != expected {
			t.Errorf("%s pragma = %q, want %q", pragma, actual, expected)
		}
	}
}

func TestOpenCreatesDatabaseDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "music.db")
	database, err := Open(path, Config{
		MmapSize:     512 * 1024 * 1024,
		CacheSizeKB:  -64000,
		MaxOpenConns: 1,
	})
	if err != nil {
		t.Fatalf("open file database: %v", err)
	}
	defer database.Close()

	var count int
	if err := database.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&count); err != nil {
		t.Fatalf("query newly opened database: %v", err)
	}
}

func TestReopenPreservesFTSIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "music.db")
	cfg := Config{
		MmapSize:     512 * 1024 * 1024,
		CacheSizeKB:  -64000,
		MaxOpenConns: 1,
	}
	ctx := context.Background()

	database, err := Open(path, cfg)
	if err != nil {
		t.Fatalf("open initial database: %v", err)
	}
	if err := Migrate(ctx, database); err != nil {
		t.Fatalf("migrate initial database: %v", err)
	}
	if _, _, err := InsertTrackWithLyrics(ctx, database, Track{
		Name: "Persistent Search Song", ArtistName: "Artist", Duration: 180,
	}, Lyrics{PlainLyrics: "persistent lyrics"}); err != nil {
		t.Fatalf("insert persistent track: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close initial database: %v", err)
	}

	database, err = Open(path, cfg)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer database.Close()
	if err := Migrate(ctx, database); err != nil {
		t.Fatalf("migrate reopened database: %v", err)
	}

	tracks, err := SearchTracks(ctx, database, "persistent search", 10)
	if err != nil {
		t.Fatalf("search reopened database: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Name != "Persistent Search Song" {
		t.Fatalf("unexpected reopened search results: %+v", tracks)
	}
}
