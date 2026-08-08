package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sillygru/music-utils/internal/db"
)

func openTestExportDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := db.Open(path, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateMetadata(context.Background(), database); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	return database
}

func TestRunExportProducesSeedDumps(t *testing.T) {
	dir := t.TempDir()
	metadataPath := filepath.Join(dir, "metadata.db")
	coverPath := filepath.Join(dir, "cover.db")
	outDir := filepath.Join(dir, "dump")

	metadataDB := openTestExportDB(t, metadataPath)
	if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
		Name: "Example Song", ArtistName: "Example Artist", AlbumName: "Example Album", Duration: 203.5,
		MetadataSource: "itunes", Source: "itunes",
	}); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}

	coverDB, err := db.Open(coverPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover database: %v", err)
	}
	t.Cleanup(func() { _ = coverDB.Close() })
	if err := db.MigrateCover(context.Background(), coverDB); err != nil {
		t.Fatalf("migrate cover database: %v", err)
	}
	if err := db.UpsertCoverArt(context.Background(), coverDB, db.CoverArtist, "Radiohead", "", "http://img/cover.jpg", "itunes"); err != nil {
		t.Fatalf("seed cover: %v", err)
	}

	if code := runExport([]string{"-metadata", metadataPath, "-cover", coverPath, "-out", outDir}); code != 0 {
		t.Fatalf("runExport exited with %d", code)
	}

	metadataDump := filepath.Join(outDir, exportMetadataFilename)
	coverDump := filepath.Join(outDir, exportCoverFilename)
	for _, path := range []string{metadataDump, coverDump} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected dump file %s: %v", path, err)
		}
	}

	// The dumps must be valid SQLite databases containing the seeded rows, so
	// pointing METADATA_DB_PATH/COVER_DB_PATH at them works as a seed.
	imported, err := db.Open(metadataDump, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open metadata dump: %v", err)
	}
	defer imported.Close()
	var count int
	if err := imported.QueryRowContext(context.Background(), "SELECT count(*) FROM tracks").Scan(&count); err != nil {
		t.Fatalf("query metadata dump: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 track in metadata dump, got %d", count)
	}

	importedCover, err := db.Open(coverDump, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover dump: %v", err)
	}
	defer importedCover.Close()
	if err := importedCover.QueryRowContext(context.Background(), "SELECT count(*) FROM cover_urls").Scan(&count); err != nil {
		t.Fatalf("query cover dump: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 cover row in cover dump, got %d", count)
	}
}
