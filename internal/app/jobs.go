package app

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/jobs"
)

// defaultMaxWriteErrors is how many consecutive failed write batches a job
// tolerates before stopping. The threshold exists for a failure that starts
// after startup, most often the database turning read-only mid-run: without it
// the job spends an entire upstream budget to persist nothing and only says so
// in a summary the operator may never see. A single failed batch is not fatal
// because the live server can briefly hold the write lock.
const defaultMaxWriteErrors = 5

// RunJobs implements `music-utils --jobs`, which lists the batch maintenance
// jobs the build provides.
func RunJobs(args []string) int {
	return RunJobsTo(os.Stdout, os.Stderr, args)
}

// RunJobsTo lists available jobs to custom output streams.
func RunJobsTo(out, errOut io.Writer, _ []string) int {
	jobs.PrintList(out)
	return 0
}

// RunJob implements `music-utils --run-job <name>`, which executes one batch
// maintenance job against the configured databases.
func RunJob(args []string) int {
	return RunJobTo(os.Stdout, os.Stderr, args)
}

// RunJobTo runs one job with custom output streams.
func RunJobTo(out, errOut io.Writer, args []string) int {
	name, rest, err := splitJobName(args)
	if err != nil {
		fmt.Fprintf(errOut, "jobs: %v\n", err)
		return 2
	}
	job, err := jobs.Lookup(name)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n\n", err)
		jobs.PrintList(errOut)
		return 2
	}

	flags := flag.NewFlagSet("run-job "+job.Name(), flag.ContinueOnError)
	flags.SetOutput(errOut)
	concurrency := flags.Int("concurrency", 4, "work items in flight")
	rate := flags.Int("rate", 0, "work items started per minute (0 = provider pacing only)")
	// A job whose units differ from the wording above restates them, so `-help`
	// describes the flag the operator is actually about to set rather than the one
	// another job would use.
	if describable, ok := job.(jobs.FlagDescriber); ok {
		for name, description := range describable.FlagDescriptions() {
			if registered := flags.Lookup(name); registered != nil {
				registered.Usage = description
			}
		}
	}
	limit := flags.Int("limit", 0, "maximum number of tracks to process (0 = no limit)")
	dryRun := flags.Bool("dry-run", false, "report what would change without writing to the databases")
	refresh := flags.String("refresh", "", "also re-fetch work a provider already settled; optionally an age (72h, 3d, 1w) so only answers older than that are refreshed. Never set: only work no provider has ever settled")
	maxWriteErrors := flags.Int("max-write-errors", defaultMaxWriteErrors, "abort after this many consecutive failed write batches (0 = never abort)")
	idleGap := flags.Duration("user-idle-gap", 0, "how long live traffic must stay quiet before the job touches a shared upstream (0 = server default)")
	metadataPath := flags.String("metadata", "", "metadata database path (defaults to METADATA_DB_PATH)")
	lyricsPath := flags.String("lyrics", "", "lyrics database path (defaults to LYRICS_DB_PATH)")
	coverPath := flags.String("cover", "", "cover database path (defaults to COVER_DB_PATH)")
	if err := flags.Parse(normalizeBareRefresh(rest)); err != nil {
		return 2
	}
	// A flag's default value cannot be told apart from one the operator typed, and
	// for -refresh the two mean different things: absent means "never-settled work
	// only", while a bare -refresh means "re-fetch everything".
	refreshOn, refreshAge := false, time.Duration(0)
	if refreshGiven(flags) {
		if refreshOn, refreshAge, err = jobs.ParseRefresh(*refresh); err != nil {
			fmt.Fprintf(errOut, "jobs: %v\n", err)
			return 2
		}
	}
	if *concurrency < 1 {
		fmt.Fprintln(errOut, "jobs: -concurrency must be at least 1")
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(errOut, "jobs: -limit cannot be negative")
		return 2
	}

	cfg := config.Load()
	if strings.TrimSpace(*metadataPath) == "" {
		*metadataPath = cfg.MetadataDBPath
	}
	if strings.TrimSpace(*lyricsPath) == "" {
		*lyricsPath = cfg.LyricsDBPath
	}
	if strings.TrimSpace(*coverPath) == "" {
		*coverPath = cfg.CoverDBPath
	}

	// A job and the live server share the same SQLite files. WAL plus the
	// database busy timeout makes that safe, but a small pool keeps the job
	// from competing with the server for write locks.
	metadataDB, err := openJobDB(*metadataPath, cfg)
	if err != nil {
		fmt.Fprintf(errOut, "jobs: %v\n", err)
		return 1
	}
	defer metadataDB.Close()

	// A job that cannot write is a no-op, but it only discovers that after
	// paging the library and spending upstream requests on every lookup. Check
	// before any work starts, and name the owner and mode so the operator knows
	// what to change. A dry run writes nothing by design, so it is exempt.
	//
	// This runs before the migration on purpose: a migration on a read-only file
	// fails with a raw SQLite error, where the probe explains the actual cause.
	if !*dryRun {
		if err := checkWritable(*metadataPath, metadataDB); err != nil {
			fmt.Fprintf(errOut, "jobs: %v\n", err)
			return 1
		}
	}

	// A job writes the same schema the server does, so it has to bring the
	// database up to date as well. Without this a job launched before the
	// server has run a new migration would fail every batch on a missing column,
	// which looks like a job fault rather than a version mismatch.
	if !*dryRun {
		migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), jobMigrateTimeout)
		err := db.MigrateMetadata(migrateCtx, metadataDB)
		cancelMigrate()
		if err != nil {
			fmt.Fprintf(errOut, "jobs: migrate metadata database: %v\n", err)
			return 1
		}
	}

	lyricsDB, err := openOptionalJobDB(*lyricsPath, cfg)
	if err != nil {
		fmt.Fprintf(errOut, "jobs: %v\n", err)
		return 1
	}
	if lyricsDB != nil {
		defer lyricsDB.Close()
		// A job writes the same schema the server does, and that includes the
		// lyrics side. Without this a job launched before the server has run the
		// lyrics migration would fail every write on a missing table, which looks
		// like a job fault rather than a version mismatch. It is cheap on a
		// migrated database because every statement is guarded.
		if !*dryRun {
			migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), jobMigrateTimeout)
			err := db.MigrateLyrics(migrateCtx, lyricsDB)
			cancelMigrate()
			if err != nil {
				fmt.Fprintf(errOut, "jobs: migrate lyrics database: %v\n", err)
				return 1
			}
		}
	}

	coverDB, err := openOptionalJobDB(*coverPath, cfg)
	if err != nil {
		fmt.Fprintf(errOut, "jobs: %v\n", err)
		return 1
	}
	if coverDB != nil {
		defer coverDB.Close()
	}

	// Ctrl-C stops the run between work units. Completed lookups are still
	// committed, and untouched tracks stay unresolved for the next run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := jobs.Options{
		Config:           cfg,
		MetadataDB:       metadataDB,
		LyricsDB:         lyricsDB,
		CoverDB:          coverDB,
		MetadataDBPath:   *metadataPath,
		LyricsDBPath:     *lyricsPath,
		Out:              out,
		ErrOut:           errOut,
		Concurrency:      *concurrency,
		RatePerMinute:    *rate,
		Limit:            *limit,
		DryRun:           *dryRun,
		Refresh:          refreshOn,
		RefreshOlderThan: refreshAge,
		UserIdleGap:      *idleGap,
		// A negative value means "no limit"; the job clamps it away.
		MaxWriteErrors: *maxWriteErrors,
	}

	runCtx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()

	if err := job.Run(runCtx, opts); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(errOut, "\nInterrupted. Work completed so far was committed; re-run to continue.")
			return 130
		}
		fmt.Fprintf(errOut, "jobs: %v\n", err)
		return 1
	}
	return 0
}

// refreshGiven reports whether -refresh appeared on the command line.
//
// It exists because the flag's default value is indistinguishable from a value the
// operator typed, and the two mean different things. Read as a string alone, the
// default "" is exactly what a bare -refresh produces, so a job given the default
// would silently re-fetch the entire library on every run.
func refreshGiven(flags *flag.FlagSet) bool {
	given := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "refresh" {
			given = true
		}
	})
	return given
}

// normalizeBareRefresh rewrites a valueless -refresh into -refresh=0 so the
// flag package does not swallow the token after it.
//
// The flag package always takes the next argument as a string flag's value, so
// "-refresh -dry-run" would otherwise be read as an age of "-dry-run" and fail to
// parse. A bare -refresh with nothing after it fails harder still, with "flag needs
// an argument", which is the most ordinary way to use the flag. Making the bare
// form explicit up front means the parser sees one shape and this stays the only
// place that has to know about the difference.
func normalizeBareRefresh(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := range out {
		if out[i] != "-refresh" && out[i] != "--refresh" {
			continue
		}
		if i+1 >= len(out) {
			// Nothing follows, so there is no value to consume.
			out[i] += "=0"
			continue
		}
		if strings.HasPrefix(out[i+1], "-") && !looksLikeAge(out[i+1]) {
			out[i] += "=0"
		}
	}
	return out
}

// looksLikeAge reports whether a dash-leading token is a value rather than a flag.
//
// This has to cover every unit jobs.ParseRefresh accepts, not just bare numbers:
// "-refresh -72h" is a malformed age that the parser will reject, and rewriting it
// into a bare refresh would instead re-fetch the entire library. The unit letters
// are restricted to digits, a decimal point, and those units, which is what
// separates a value from every flag the command defines.
func looksLikeAge(token string) bool {
	body := strings.TrimLeft(token, "-")
	if body == "" {
		return false
	}
	for _, r := range body {
		switch {
		case r >= '0' && r <= '9':
		case r == '.', r == 'h', r == 'm', r == 's', r == 'd', r == 'w':
		default:
			return false
		}
	}
	// A token of only unit letters is not a value.
	return strings.ContainsAny(body, "0123456789")
}

// splitJobName pulls the job name off the front of the arguments, so flags may
// follow it directly: --run-job metadata-backfill -concurrency 8. The name must
// come first; a leading flag is rejected rather than guessed at.
func splitJobName(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("--run-job needs a job name (see --jobs)")
	}
	if strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("--run-job needs a job name before its flags (see --jobs)")
	}
	return args[0], args[1:], nil
}

func openJobDB(path string, cfg config.Config) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("database path is empty")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("cannot open database %s: %w", path, err)
	}
	return db.Open(path, db.Config{
		MmapSize:        cfg.DBMmapSize,
		CacheSizeKB:     cfg.DBCacheSizeKB,
		MaxOpenConns:    2,
		TxLockImmediate: true,
	})
}

// openOptionalJobDB opens a database only when the file exists. Jobs that do
// not need a given database should not fail because it is absent.
func openOptionalJobDB(path string, cfg config.Config) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	return openJobDB(path, cfg)
}

// checkWritable verifies the metadata database accepts a write, and explains a
// failure in terms the operator can act on.
//
// The probe itself lives in internal/db so a job can run it against a database
// other than this one; only the message needs the path, so only that stays here.
func checkWritable(path string, database *sql.DB) error {
	// Bound the probe so a database held under a long write transaction by the
	// live server reports as unusable instead of blocking the job's startup.
	ctx, cancel := context.WithTimeout(context.Background(), writeProbeTimeout)
	defer cancel()

	if err := db.ProbeWritable(ctx, database); err != nil {
		return unwritableError(path, err)
	}
	return nil
}

// unwritableError explains a failed write probe, naming the owner and the
// directory so the operator can act without reading the source.
func unwritableError(path string, err error) error {
	dir := dirOf(path)
	return fmt.Errorf("metadata database %s is not writable: %w\n"+
		"  this job's only effect is to write rows, so there is nothing for it to do.\n"+
		"  it is owned by %s, and SQLite also writes -wal and -shm files in %s,\n"+
		"  so the job needs write access to both the file and that directory.\n"+
		"  either run the job as the owning user, or grant access with:\n"+
		"    sudo chgrp -R music-utils %s\n"+
		"    sudo chmod 664 %s/*.db %s/*.db-wal %s/*.db-shm\n"+
		"    sudo chmod 2775 %s",
		path, err, ownerOf(path), dir, dir, dir, dir, dir, dir)
}

// writeProbeTimeout bounds the writability check at job startup.
const writeProbeTimeout = 5 * time.Second

// jobMigrateTimeout bounds the schema migration a job performs at startup. It
// normally completes in well under a second, but the migration rebuilds the
// full-text index when a column changes, and that scales with library size. A
// bound keeps a job from appearing to hang on a large database while the live
// server holds the write lock.
const jobMigrateTimeout = 2 * time.Minute

// dirOf reports the directory holding a path, for error messages about the
// SQLite -wal and -shm files created alongside it.
func dirOf(path string) string {
	dir := filepath.Dir(path)
	if dir == "" {
		return "."
	}
	return dir
}
