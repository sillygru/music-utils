package app

import (
	"bytes"
	"context"
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
