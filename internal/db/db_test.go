package db

import (
	"context"
	"database/sql"
	"errors"
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

func TestCoverArtUpsertAndFind(t *testing.T) {
	ctx := context.Background()
	database, err := Open(":memory:", Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover database: %v", err)
	}
	defer database.Close()
	if err := MigrateCover(ctx, database); err != nil {
		t.Fatalf("migrate cover database: %v", err)
	}

	if _, err := FindCoverArt(ctx, database, CoverArtist, "Radiohead", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected no rows before write, got %v", err)
	}

	if err := UpsertCoverArt(ctx, database, CoverArtist, "Radiohead", "", "http://img/a.jpg", "itunes"); err != nil {
		t.Fatalf("upsert artist cover: %v", err)
	}
	if err := UpsertCoverArt(ctx, database, CoverAlbum, "Radiohead", "OK Computer", "http://img/b.jpg", "deezer"); err != nil {
		t.Fatalf("upsert album cover: %v", err)
	}

	artist, err := FindCoverArt(ctx, database, CoverArtist, "radiohead", "")
	if err != nil {
		t.Fatalf("find artist cover: %v", err)
	}
	if artist.CoverURL != "http://img/a.jpg" || artist.CoverSource != "itunes" {
		t.Fatalf("unexpected artist cover: %+v", artist)
	}

	album, err := FindCoverArt(ctx, database, CoverAlbum, "Radiohead", "OK Computer")
	if err != nil {
		t.Fatalf("find album cover: %v", err)
	}
	if album.CoverURL != "http://img/b.jpg" || album.CoverSource != "deezer" {
		t.Fatalf("unexpected album cover: %+v", album)
	}

	if err := UpsertCoverArt(ctx, database, CoverArtist, "Lost Artist", "", "", ""); err != nil {
		t.Fatalf("upsert artist negative: %v", err)
	}
	negative, err := FindCoverArt(ctx, database, CoverArtist, "Lost Artist", "")
	if err != nil {
		t.Fatalf("find negative artist cover: %v", err)
	}
	if negative.CoverURL != "" {
		t.Fatalf("expected empty cover URL, got %q", negative.CoverURL)
	}
}

func TestCoverArtReupsertUpdatesNotDuplicates(t *testing.T) {
	ctx := context.Background()
	database, err := Open(":memory:", Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover database: %v", err)
	}
	defer database.Close()
	if err := MigrateCover(ctx, database); err != nil {
		t.Fatalf("migrate cover database: %v", err)
	}

	// Artist rows store '' for the album column; re-upserting the same artist
	// must update the single row rather than inserting a duplicate (SQLite
	// UNIQUE indexes treat NULLs as distinct, so NULL album rows never
	// conflict).
	if err := UpsertCoverArt(ctx, database, CoverArtist, "Radiohead", "", "http://img/old.jpg", "itunes"); err != nil {
		t.Fatalf("first artist upsert: %v", err)
	}
	if err := UpsertCoverArt(ctx, database, CoverArtist, "Radiohead", "", "http://img/new.jpg", "deezer"); err != nil {
		t.Fatalf("second artist upsert: %v", err)
	}

	var count int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM cover_urls WHERE entity_type = 'artist' AND artist_name_lower = 'radiohead'`).Scan(&count); err != nil {
		t.Fatalf("count artist rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one artist row after re-upsert, got %d", count)
	}
	row, err := FindCoverArt(ctx, database, CoverArtist, "Radiohead", "")
	if err != nil {
		t.Fatalf("find re-upserted artist cover: %v", err)
	}
	if row.CoverURL != "http://img/new.jpg" || row.CoverSource != "deezer" {
		t.Fatalf("expected the re-upsert to replace the URL, got %+v", row)
	}
}

func TestCoverArtVariantsUpsertFindAndReplace(t *testing.T) {
	ctx := context.Background()
	database, err := Open(":memory:", Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover database: %v", err)
	}
	defer database.Close()
	if err := MigrateCover(ctx, database); err != nil {
		t.Fatalf("migrate cover database: %v", err)
	}

	if err := UpsertCoverArtVariants(ctx, database, CoverArtist, "Radiohead", "", []CoverVariant{
		{URL: "http://img/lastfm.jpg", Source: "lastfm"},
		{URL: "http://img/itunes.jpg", Source: "itunes"},
		{URL: "http://img/deezer.jpg", Source: "deezer"},
	}); err != nil {
		t.Fatalf("upsert variants: %v", err)
	}

	row, err := FindCoverArt(ctx, database, CoverArtist, "Radiohead", "")
	if err != nil {
		t.Fatalf("find winner: %v", err)
	}
	if row.CoverURL != "http://img/lastfm.jpg" || row.CoverSource != "lastfm" {
		t.Fatalf("expected the first variant to be the winner, got %+v", row)
	}

	variants, err := FindCoverVariants(ctx, database, row.ID)
	if err != nil {
		t.Fatalf("find variants: %v", err)
	}
	if len(variants) != 3 {
		t.Fatalf("expected 3 variants, got %d", len(variants))
	}
	for i, want := range []string{"http://img/lastfm.jpg", "http://img/itunes.jpg", "http://img/deezer.jpg"} {
		if variants[i].URL != want || variants[i].Rank != i {
			t.Fatalf("variant %d = %+v, want %s at rank %d", i, variants[i], want, i)
		}
	}

	// Re-upserting replaces the whole set, winner included.
	if err := UpsertCoverArtVariants(ctx, database, CoverArtist, "Radiohead", "", []CoverVariant{
		{URL: "http://img/new.jpg", Source: "itunes"},
	}); err != nil {
		t.Fatalf("replace variants: %v", err)
	}
	row, err = FindCoverArt(ctx, database, CoverArtist, "Radiohead", "")
	if err != nil {
		t.Fatalf("find replaced winner: %v", err)
	}
	if row.CoverURL != "http://img/new.jpg" {
		t.Fatalf("expected the re-upsert to replace the winner, got %q", row.CoverURL)
	}
	variants, err = FindCoverVariants(ctx, database, row.ID)
	if err != nil {
		t.Fatalf("find replaced variants: %v", err)
	}
	if len(variants) != 1 || variants[0].URL != "http://img/new.jpg" || variants[0].Rank != 0 {
		t.Fatalf("expected a single replacement variant, got %+v", variants)
	}

	// A miss (no variants) memoizes a negative row with an empty variant set.
	if err := UpsertCoverArtVariants(ctx, database, CoverAlbum, "Radiohead", "OK Computer", nil); err != nil {
		t.Fatalf("upsert negative: %v", err)
	}
	negative, err := FindCoverArt(ctx, database, CoverAlbum, "Radiohead", "OK Computer")
	if err != nil {
		t.Fatalf("find negative: %v", err)
	}
	if negative.CoverURL != "" {
		t.Fatalf("expected empty cover URL, got %q", negative.CoverURL)
	}
	negVariants, err := FindCoverVariants(ctx, database, negative.ID)
	if err != nil {
		t.Fatalf("find negative variants: %v", err)
	}
	if len(negVariants) != 0 {
		t.Fatalf("expected no variants on a negative row, got %+v", negVariants)
	}
}

func TestPromoteCoverVariant(t *testing.T) {
	ctx := context.Background()
	database, err := Open(":memory:", Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover database: %v", err)
	}
	defer database.Close()
	if err := MigrateCover(ctx, database); err != nil {
		t.Fatalf("migrate cover database: %v", err)
	}

	if err := UpsertCoverArtVariants(ctx, database, CoverArtist, "Radiohead", "", []CoverVariant{
		{URL: "http://img/a.jpg", Source: "lastfm"},
		{URL: "http://img/b.jpg", Source: "itunes"},
		{URL: "http://img/c.jpg", Source: "deezer"},
	}); err != nil {
		t.Fatalf("upsert variants: %v", err)
	}
	row, err := FindCoverArt(ctx, database, CoverArtist, "Radiohead", "")
	if err != nil {
		t.Fatalf("find winner: %v", err)
	}

	if err := PromoteCoverVariant(ctx, database, row.ID, "http://img/b.jpg", "itunes", 1); err != nil {
		t.Fatalf("promote variant: %v", err)
	}

	row, err = FindCoverArt(ctx, database, CoverArtist, "Radiohead", "")
	if err != nil {
		t.Fatalf("find promoted winner: %v", err)
	}
	if row.CoverURL != "http://img/b.jpg" || row.CoverSource != "itunes" {
		t.Fatalf("expected the promoted URL to become the winner, got %+v", row)
	}
	variants, err := FindCoverVariants(ctx, database, row.ID)
	if err != nil {
		t.Fatalf("find variants after promotion: %v", err)
	}
	if len(variants) != 3 || variants[0].URL != "http://img/b.jpg" || variants[0].Rank != 0 || variants[1].URL != "http://img/a.jpg" || variants[1].Rank != 1 {
		t.Fatalf("unexpected variant order after promotion: %+v", variants)
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
	if err := MigrateMetadata(ctx, database); err != nil {
		t.Fatalf("migrate initial metadata database: %v", err)
	}
	lyricsDB, err := Open(":memory:", cfg)
	if err != nil {
		t.Fatalf("open initial lyrics database: %v", err)
	}
	defer lyricsDB.Close()
	if err := MigrateLyrics(ctx, lyricsDB); err != nil {
		t.Fatalf("migrate initial lyrics database: %v", err)
	}
	if _, _, err := InsertTrackWithLyrics(ctx, database, lyricsDB, Track{
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
	if err := MigrateMetadata(ctx, database); err != nil {
		t.Fatalf("migrate reopened metadata database: %v", err)
	}

	tracks, err := SearchTracks(ctx, database, lyricsDB, "persistent search", 10)
	if err != nil {
		t.Fatalf("search reopened database: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Name != "Persistent Search Song" {
		t.Fatalf("unexpected reopened search results: %+v", tracks)
	}
}
