package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/jobs"
)

func TestRunJobsListsRegisteredJobs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := RunJobsTo(&out, &errOut, nil); code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "metadata-backfill") {
		t.Fatalf("expected the job catalog to list metadata-backfill, got:\n%s", text)
	}
	if !strings.Contains(text, "--run-job") {
		t.Fatalf("expected usage guidance, got:\n%s", text)
	}
}

func TestRunJobRejectsUnknownJob(t *testing.T) {
	var out, errOut bytes.Buffer
	code := RunJobTo(&out, &errOut, []string{"does-not-exist"})
	if code != 2 {
		t.Fatalf("expected exit 2 for an unknown job, got %d", code)
	}
	// The error should help the operator by listing what does exist.
	if !strings.Contains(errOut.String(), "metadata-backfill") {
		t.Errorf("expected the error to list available jobs, got:\n%s", errOut.String())
	}
}

func TestRunJobRequiresJobName(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-concurrency", "2"},
	} {
		var out, errOut bytes.Buffer
		if code := RunJobTo(&out, &errOut, args); code != 2 {
			t.Errorf("args %v: expected exit 2, got %d", args, code)
		}
		if !strings.Contains(errOut.String(), "job name") {
			t.Errorf("args %v: expected a helpful message, got %q", args, errOut.String())
		}
	}
}

func TestSplitJobName(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantName  string
		wantRest  []string
		wantError bool
	}{
		{name: "name only", args: []string{"metadata-backfill"}, wantName: "metadata-backfill", wantRest: []string{}},
		{
			name: "name then flags", args: []string{"metadata-backfill", "-dry-run", "-limit", "5"},
			wantName: "metadata-backfill", wantRest: []string{"-dry-run", "-limit", "5"},
		},
		{
			name: "flags before name is rejected", args: []string{"-dry-run", "metadata-backfill"},
			wantError: true,
		},
		{name: "flags first is an error", args: []string{"-dry-run"}, wantError: true},
		{name: "no args is an error", args: nil, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, rest, err := splitJobName(tc.args)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected an error, got name=%q rest=%v", name, rest)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if len(rest) != len(tc.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
			for i := range rest {
				if rest[i] != tc.wantRest[i] {
					t.Errorf("rest = %v, want %v", rest, tc.wantRest)
				}
			}
		})
	}
}

func TestRunJobReportsMissingDatabase(t *testing.T) {
	var out, errOut bytes.Buffer
	code := RunJobTo(&out, &errOut, []string{
		"metadata-backfill",
		"-metadata", filepath.Join(t.TempDir(), "absent.db"),
		"-dry-run",
	})
	if code != 1 {
		t.Fatalf("expected exit 1 when the database is missing, got %d", code)
	}
	if !strings.Contains(errOut.String(), "cannot open database") {
		t.Errorf("expected a clear database error, got:\n%s", errOut.String())
	}
}

func TestRunJobRefusesUnwritableDatabase(t *testing.T) {
	// A job whose only effect is to write has nothing to do against a database
	// it cannot write. It must say so before spending upstream requests,
	// rather than reporting progress for a run that persisted nothing.
	dir := t.TempDir()
	metadataPath := filepath.Join(dir, "metadata.db")

	metadataDB, err := db.Open(metadataPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open metadata: %v", err)
	}
	if err := db.MigrateMetadata(context.Background(), metadataDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
			Name: "Unresolved", ArtistName: "Artist", AlbumName: "Album",
			Duration: 200, Source: "lrclib_fallback",
		}); err != nil {
			t.Fatalf("seed track: %v", err)
		}
	}
	if err := metadataDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Removing write permission for the owner is the portable way to make a
	// SQLite file unwritable, but it does not stop root, so skip rather than
	// assert the wrong outcome.
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not prevent writes")
	}
	if err := os.Chmod(metadataPath, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(metadataPath, 0o644) })

	var out, errOut bytes.Buffer
	code := RunJobTo(&out, &errOut, []string{
		"metadata-backfill",
		"-metadata", metadataPath,
		"-limit", "3",
	})
	if code != 1 {
		t.Fatalf("expected exit 1 for an unwritable database, got %d (stderr: %s)", code, errOut.String())
	}
	// The message has to name the problem, not just fail.
	if !strings.Contains(errOut.String(), "not writable") {
		t.Errorf("expected a writability error, got:\n%s", errOut.String())
	}
	// It must not have started the run: no progress frame means no lookups.
	if strings.Contains(out.String(), "Metadata backfill") {
		t.Errorf("expected the job to refuse before starting, got:\n%s", out.String())
	}
}

func TestRunJobDryRunIgnoresWritePermission(t *testing.T) {
	// A dry run writes nothing by design, so an unwritable file is a valid
	// thing to report against and must not be refused.
	dir := t.TempDir()
	metadataPath := filepath.Join(dir, "metadata.db")

	metadataDB, err := db.Open(metadataPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open metadata: %v", err)
	}
	if err := db.MigrateMetadata(context.Background(), metadataDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
		Name: "Unresolved", ArtistName: "Artist", AlbumName: "Album",
		Duration: 200, Source: "lrclib_fallback",
	}); err != nil {
		t.Fatalf("seed track: %v", err)
	}
	if err := metadataDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not prevent writes")
	}
	if err := os.Chmod(metadataPath, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(metadataPath, 0o644) })

	var out, errOut bytes.Buffer
	RunJobTo(&out, &errOut, []string{
		"metadata-backfill",
		"-metadata", metadataPath,
		"-dry-run",
		"-limit", "1",
	})
	if strings.Contains(errOut.String(), "not writable") {
		t.Errorf("a dry run must not be refused for write permission, got:\n%s", errOut.String())
	}
	if !strings.Contains(out.String(), "Metadata backfill") {
		t.Errorf("expected the dry run to start, got:\n%s", out.String())
	}
}

func TestRunJobMigratesMetadataSchema(t *testing.T) {
	// The job writes the same schema the server does, so it has to apply the
	// migration itself. A job started before the server has run a new migration
	// would otherwise fail every batch on a missing column, which reads as a job
	// fault rather than a version mismatch.
	dir := t.TempDir()
	metadataPath := filepath.Join(dir, "metadata.db")

	legacy, err := db.Open(metadataPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.MigrateMetadata(context.Background(), legacy); err != nil {
		t.Fatalf("migrate fresh: %v", err)
	}
	// Stand in for a database written before the newest column existed. The age
	// index covers that column and SQLite refuses to drop a column an index
	// depends on; a genuinely pre-upgrade database predates both.
	if _, err := legacy.ExecContext(context.Background(), "DROP INDEX IF EXISTS idx_tracks_metadata_age"); err != nil {
		t.Fatalf("drop age index: %v", err)
	}
	if _, err := legacy.ExecContext(context.Background(), "ALTER TABLE tracks DROP COLUMN metadata_checked_at"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	// One already-resolved track and nothing pending, so the job returns before
	// any provider is contacted and the test makes no network call.
	if _, err := legacy.ExecContext(context.Background(),
		"UPDATE tracks SET metadata_checked = 1"); err != nil {
		t.Fatalf("mark checked: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out, errOut bytes.Buffer
	RunJobTo(&out, &errOut, []string{"metadata-backfill", "-metadata", metadataPath})
	if strings.Contains(errOut.String(), "migrate") {
		t.Fatalf("expected the job to migrate cleanly, got:\n%s", errOut.String())
	}

	check, err := db.Open(metadataPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer check.Close()
	var count int
	if err := check.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pragma_table_info('tracks') WHERE name = 'metadata_checked_at'").Scan(&count); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if count != 1 {
		t.Error("expected the job to add metadata_checked_at to an existing database")
	}
	// Rows checked before the column existed are stamped rather than left NULL,
	// so the column is uniformly populated.
	var nulls int
	if err := check.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM tracks WHERE metadata_checked = 1 AND metadata_checked_at IS NULL").Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 0 {
		t.Errorf("expected already-checked rows to be stamped, %d still NULL", nulls)
	}
}

func TestRunJobDryRunLeavesDatabaseUnchanged(t *testing.T) {
	dir := t.TempDir()
	metadataPath := filepath.Join(dir, "metadata.db")

	metadataDB, err := db.Open(metadataPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open metadata: %v", err)
	}
	if err := db.MigrateMetadata(context.Background(), metadataDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// One unresolved track, so the job has work to consider.
	if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
		Name: "Unresolved", ArtistName: "Artist", AlbumName: "Album",
		Duration: 200, Source: "lrclib_fallback",
	}); err != nil {
		t.Fatalf("seed track: %v", err)
	}
	if err := metadataDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	before, err := os.Stat(metadataPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	_ = before

	var out, errOut bytes.Buffer
	// A dry run must never write rows. Note the database file size is not a
	// valid signal here: merely opening a WAL database grows the main file as
	// SQLite allocates its WAL and shared-memory structures, so only the row
	// state is asserted below.
	code := RunJobTo(&out, &errOut, []string{
		"metadata-backfill",
		"-metadata", metadataPath,
		"-dry-run",
		"-limit", "1",
	})
	// Exit status depends on whether a provider is reachable, so only assert
	// that the run did not modify any row.
	_ = code

	// The track must still be unresolved.
	check, err := db.Open(metadataPath, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer check.Close()
	remaining, err := db.CountTracksMissingMetadata(context.Background(), check)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("expected the dry run to leave the track unresolved, %d remain", remaining)
	}
}

func TestJobsOptionsAreWiredFromFlags(t *testing.T) {
	// Guards the flag names documented by the job's Flags() string.
	job, err := jobs.Lookup("metadata-backfill")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	flags := job.Flags()
	for _, name := range []string{"-concurrency", "-rate", "-limit", "-dry-run"} {
		if !strings.Contains(flags, name) {
			t.Errorf("job advertises flags %q but %q is not documented", flags, name)
		}
	}
}
