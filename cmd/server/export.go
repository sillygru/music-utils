package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
)

const (
	exportMetadataFilename = "metadata-dump.sqlite3"
	exportCoverFilename    = "cover-dump.sqlite3"
)

// runExport implements `music-utils export`, which produces redistributable
// seed dumps of the metadata and cover databases using SQLite's VACUUM INTO.
// Lyrics are intentionally excluded: full lyrics are copyrighted content owned
// by others and are available directly from LRCLIB, so a dump here must never
// redistribute them.
func runExport(args []string) int {
	flags := flag.NewFlagSet("export", flag.ExitOnError)
	metadataPath := flags.String("metadata", "", "metadata database path (defaults to METADATA_DB_PATH)")
	coverPath := flags.String("cover", "", "cover database path (defaults to COVER_DB_PATH)")
	outDir := flags.String("out", "dump", "output directory for the dump files")
	_ = flags.Parse(args)

	cfg := config.Load()
	if strings.TrimSpace(*metadataPath) == "" {
		*metadataPath = cfg.MetadataDBPath
	}
	if strings.TrimSpace(*coverPath) == "" {
		*coverPath = cfg.CoverDBPath
	}
	if strings.TrimSpace(*outDir) == "" {
		*outDir = "dump"
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "export: create output directory: %v\n", err)
		return 1
	}

	ctx := context.Background()
	if err := exportDatabase(ctx, *metadataPath, filepath.Join(*outDir, exportMetadataFilename), "metadata"); err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		return 1
	}
	if err := exportDatabase(ctx, *coverPath, filepath.Join(*outDir, exportCoverFilename), "cover"); err != nil {
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		return 1
	}

	fmt.Printf("export: wrote %s\n", filepath.Join(*outDir, exportMetadataFilename))
	fmt.Printf("export: wrote %s\n", filepath.Join(*outDir, exportCoverFilename))
	fmt.Printf("export: lyrics are intentionally excluded; point LRCLIB_BASE_URL at lrclib.net (or a self-hosted LRCLIB) instead\n")
	return 0
}

// exportDatabase opens source and writes a compact VACUUM INTO copy to dest.
// The destination schema matches the source, so pointing METADATA_DB_PATH or
// COVER_DB_PATH at a dump file works as a seed.
func exportDatabase(ctx context.Context, source, dest, label string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("%s database path is empty", label)
	}
	// Fail on a missing source instead of letting db.Open create an empty
	// database and silently exporting an empty dump.
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("open %s database %s: %w", label, source, err)
	}
	database, err := db.Open(source, db.Config{
		MmapSize:     512 * 1024 * 1024,
		CacheSizeKB:  -64000,
		MaxOpenConns: 1,
	})
	if err != nil {
		return fmt.Errorf("open %s database: %w", label, err)
	}
	defer database.Close()

	// A checkpoint makes the dump deterministic even when the source is a
	// live WAL-mode database. Best effort: a busy database simply leaves the
	// dump to VACUUM INTO's own snapshot.
	_, _ = database.ExecContext(ctx, "PRAGMA wal_checkpoint(FULL)")

	// VACUUM INTO does not accept bound parameters; escape single quotes in
	// the destination path before embedding it in the statement.
	escaped := strings.ReplaceAll(dest, "'", "''")
	if _, err := database.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		return fmt.Errorf("dump %s database: %w", label, err)
	}
	return nil
}
