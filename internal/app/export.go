package app

import (
	"context"
	"flag"
	"fmt"
	"io"
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

// RunExport implements `music-utils export`, which produces redistributable
// seed dumps of the metadata and cover databases using SQLite's VACUUM INTO.
func RunExport(args []string) int {
	return RunExportTo(os.Stdout, os.Stderr, args)
}

// RunExportTo runs the export subcommand with custom output streams.
func RunExportTo(out, errOut io.Writer, args []string) int {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(errOut)
	metadataPath := flags.String("metadata", "", "metadata database path (defaults to METADATA_DB_PATH)")
	coverPath := flags.String("cover", "", "cover database path (defaults to COVER_DB_PATH)")
	outDir := flags.String("out", "dump", "output directory for the dump files")
	if err := flags.Parse(args); err != nil {
		return 1
	}

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
		fmt.Fprintf(errOut, "export: create output directory: %v\n", err)
		return 1
	}

	ctx := context.Background()
	if err := exportDatabase(ctx, *metadataPath, filepath.Join(*outDir, exportMetadataFilename), "metadata"); err != nil {
		fmt.Fprintf(errOut, "export: %v\n", err)
		return 1
	}
	if err := exportDatabase(ctx, *coverPath, filepath.Join(*outDir, exportCoverFilename), "cover"); err != nil {
		fmt.Fprintf(errOut, "export: %v\n", err)
		return 1
	}

	fmt.Fprintf(out, "export: wrote %s\n", filepath.Join(*outDir, exportMetadataFilename))
	fmt.Fprintf(out, "export: wrote %s\n", filepath.Join(*outDir, exportCoverFilename))
	fmt.Fprintf(out, "export: lyrics and the request log are intentionally excluded; point LRCLIB_BASE_URL at lrclib.net (or a self-hosted LRCLIB) instead\n")
	return 0
}

func exportDatabase(ctx context.Context, source, dest, label string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("%s database path is empty", label)
	}
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

	_, _ = database.ExecContext(ctx, "PRAGMA wal_checkpoint(FULL)")

	escaped := strings.ReplaceAll(dest, "'", "''")
	if _, err := database.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		return fmt.Errorf("dump %s database: %w", label, err)
	}
	return nil
}
