package app

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sillygru/music-utils/internal/db"
)

func TestRunExportProducesSeedDumps(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "metadata.db")
	coverPath := filepath.Join(dir, "cover.db")
	outDir := filepath.Join(dir, "dump")

	metaDB, err := db.Open(metaPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open meta: %v", err)
	}
	if err := db.MigrateMetadata(context.Background(), metaDB); err != nil {
		t.Fatalf("migrate meta: %v", err)
	}
	_ = metaDB.Close()

	coverDB, err := db.Open(coverPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover: %v", err)
	}
	if err := db.MigrateCover(context.Background(), coverDB); err != nil {
		t.Fatalf("migrate cover: %v", err)
	}
	_ = coverDB.Close()

	var out, errOut bytes.Buffer
	code := RunExportTo(&out, &errOut, []string{
		"-metadata", metaPath,
		"-cover", coverPath,
		"-out", outDir,
	})
	if code != 0 {
		t.Fatalf("expected export exit 0, got %d (stderr: %s)", code, errOut.String())
	}

	for _, name := range []string{exportMetadataFilename, exportCoverFilename} {
		p := filepath.Join(outDir, name)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected dump file %s to exist: %v", p, err)
		}
	}
}

// A dump carries resolved content, not one machine's recent query history. The
// cover search cache speeds up re-querying a live instance but is not seed data,
// so it is cleared from the copy — and, just as importantly, left intact in the
// source the server is still using.
func TestExportDumpsExcludeOperationalCaches(t *testing.T) {
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "metadata.db")
	coverPath := filepath.Join(dir, "cover.db")
	outDir := filepath.Join(dir, "dump")

	metaDB, err := db.Open(metaPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open meta: %v", err)
	}
	if err := db.MigrateMetadata(context.Background(), metaDB); err != nil {
		t.Fatalf("migrate meta: %v", err)
	}
	_ = metaDB.Close()

	coverDB, err := db.Open(coverPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover: %v", err)
	}
	if err := db.MigrateCover(context.Background(), coverDB); err != nil {
		t.Fatalf("migrate cover: %v", err)
	}
	// One resolved cover to keep, and one cached search to drop.
	if err := db.UpsertCoverArt(context.Background(), coverDB, db.CoverAlbum, "Artist", "Album", "http://img/a.jpg", "itunes"); err != nil {
		t.Fatalf("seed cover: %v", err)
	}
	if err := db.UpsertCoverSearchCache(context.Background(), coverDB, "some query", []byte(`[{"coverUrl":"http://img/a.jpg"}]`)); err != nil {
		t.Fatalf("seed search cache: %v", err)
	}
	_ = coverDB.Close()

	var out, errOut bytes.Buffer
	if code := RunExportTo(&out, &errOut, []string{"-metadata", metaPath, "-cover", coverPath, "-out", outDir}); code != 0 {
		t.Fatalf("expected export exit 0, got %d (stderr: %s)", code, errOut.String())
	}

	dump := openDump(t, filepath.Join(outDir, exportCoverFilename))
	if got := countRows(t, dump, "cover_urls"); got != 1 {
		t.Fatalf("dump has %d cover_urls rows, want the 1 resolved cover", got)
	}
	if got := countRows(t, dump, "cover_search_cache"); got != 0 {
		t.Fatalf("dump has %d cover_search_cache rows, want 0", got)
	}

	// The live database keeps its cache; only the copy is cleaned.
	source := openDump(t, coverPath)
	if got := countRows(t, source, "cover_search_cache"); got != 1 {
		t.Fatalf("source has %d cover_search_cache rows, want the original 1", got)
	}
}

func openDump(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := db.Open(path, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func countRows(t *testing.T, database *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}
