package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Coordination between the live server and background jobs happens through two
// small tables in the metadata database, which is the one file both processes
// already share.
//
// upstream_pacer holds a cross-process lease per upstream host. A caller claims
// the next free slot inside an IMMEDIATE transaction, so the server and a
// running job take turns instead of each applying its own private pacing and
// doubling the real request rate. last_user_at records live traffic, which is
// what lets a job tell "the server is busy" from "the server is idle".
//
// job_coordination carries a metadata revision counter and the name of the job
// currently running. The job bumps the revision whenever it commits, and the
// server watches it so it can drop memoized misses that the job has since
// resolved.
const coordinationSchema = `
CREATE TABLE IF NOT EXISTS upstream_pacer (
	name TEXT PRIMARY KEY,
	next_at INTEGER NOT NULL DEFAULT 0,
	user_next_at INTEGER NOT NULL DEFAULT 0,
	last_user_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS job_coordination (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	metadata_revision INTEGER NOT NULL DEFAULT 0,
	active_job TEXT NOT NULL DEFAULT '',
	job_started_at DATETIME,
	updated_at DATETIME,
	revision_bumped_at INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO job_coordination (id, metadata_revision, active_job) VALUES (1, 0, '');
`

// revisionBumpInterval is the shortest gap between two revision bumps.
//
// A long-running job commits a batch every couple of seconds, and the server
// invalidates its whole memoized cache whenever it sees the revision move.
// Bumping on every batch therefore means a multi-hour backfill keeps discarding
// a cache whose misses are meant to survive for 24 hours, which is the opposite
// of what the coordination is for. Throttling keeps the signal — a job that
// wrote something is noticed within the interval — without the churn.
const revisionBumpInterval = 30 * time.Second

// EnsureCoordination creates the coordination tables if they are absent. It is
// idempotent and safe to call concurrently from several processes, so the
// server, a job, and tests can all call it at startup.
func EnsureCoordination(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("metadata database is nil")
	}
	if _, err := database.ExecContext(ctx, coordinationSchema); err != nil {
		return fmt.Errorf("ensure coordination tables: %w", err)
	}
	// Add columns introduced after a table may already exist, so an older
	// coordination table is upgraded in place instead of forcing a rebuild.
	for _, column := range []string{
		"user_next_at INTEGER NOT NULL DEFAULT 0",
	} {
		if err := addColumnIfMissing(ctx, database, "upstream_pacer", column); err != nil {
			return err
		}
	}
	if err := addColumnIfMissing(ctx, database, "job_coordination", "revision_bumped_at INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

// addColumnIfMissing adds a column to an existing table, ignoring the case
// where it is already present.
func addColumnIfMissing(ctx context.Context, database *sql.DB, table, column string) error {
	name := strings.Fields(column)[0]
	var count int
	if err := database.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, name).Scan(&count); err != nil {
		return fmt.Errorf("inspect %s.%s: %w", table, name, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := database.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, name, err)
	}
	return nil
}

// CoordinationState is the shared view of job activity.
type CoordinationState struct {
	// MetadataRevision increases every time a job commits metadata, letting
	// readers detect that memoized data may be stale.
	MetadataRevision int64
	// RevisionBumpedAt is the unix-millisecond time the revision counter was
	// last advanced as far as readers are concerned. A job that commits
	// continuously advances MetadataRevision on every batch but holds this
	// value steady within the throttle window, so a reader that watches this
	// field is notified periodically instead of continuously.
	RevisionBumpedAt int64
	// ActiveJob names the job currently running, or is empty when idle.
	ActiveJob string
	// JobStartedAt is when the active job began, or nil when idle.
	JobStartedAt sql.NullString
	// UpdatedAt is when this row last changed.
	UpdatedAt sql.NullString
}

// ReadCoordination returns the current coordination state.
func ReadCoordination(ctx context.Context, database *sql.DB) (CoordinationState, error) {
	if database == nil {
		return CoordinationState{}, errors.New("metadata database is nil")
	}
	var state CoordinationState
	var bumpedAt sql.NullInt64
	err := database.QueryRowContext(ctx,
		`SELECT metadata_revision, revision_bumped_at, active_job, job_started_at, updated_at
FROM job_coordination WHERE id = 1`).Scan(
		&state.MetadataRevision, &bumpedAt, &state.ActiveJob, &state.JobStartedAt, &state.UpdatedAt)
	state.RevisionBumpedAt = bumpedAt.Int64
	if errors.Is(err, sql.ErrNoRows) {
		// The table exists but the singleton row does not; treat it as idle
		// rather than failing, so a partially migrated database still works.
		return CoordinationState{}, nil
	}
	if err != nil {
		return CoordinationState{}, fmt.Errorf("read coordination state: %w", err)
	}
	return state, nil
}

// BeginJob records that a job has started, so the server can report it.
func BeginJob(ctx context.Context, database *sql.DB, name string) error {
	if database == nil {
		return errors.New("metadata database is nil")
	}
	if _, err := database.ExecContext(ctx, `UPDATE job_coordination SET
active_job = ?, job_started_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
WHERE id = 1`, name); err != nil {
		return fmt.Errorf("begin job: %w", err)
	}
	return nil
}

// EndJob clears the active job marker, leaving the revision untouched.
func EndJob(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return errors.New("metadata database is nil")
	}
	if _, err := database.ExecContext(ctx, `UPDATE job_coordination SET
active_job = '', updated_at = CURRENT_TIMESTAMP WHERE id = 1`); err != nil {
		return fmt.Errorf("end job: %w", err)
	}
	return nil
}

// BumpMetadataRevision advances the revision counter and reports the new value.
// The server compares it against the last value it saw to decide whether to
// drop its memoized metadata.
//
// Bumps are rate limited to one per revisionBumpInterval, because the server
// drops its entire cache on every change and a job that commits continuously
// would otherwise keep it permanently empty. Within the window the counter is
// still advanced, so no write is silently lost to a later reader: only the
// notification is coalesced.
func BumpMetadataRevision(ctx context.Context, database *sql.DB) (int64, error) {
	if database == nil {
		return 0, errors.New("metadata database is nil")
	}
	now := time.Now().UnixMilli()
	var revision int64
	err := database.QueryRowContext(ctx, `UPDATE job_coordination SET
metadata_revision = metadata_revision + 1,
revision_bumped_at = CASE
	WHEN revision_bumped_at <= ? THEN ?
	ELSE revision_bumped_at
END,
updated_at = CURRENT_TIMESTAMP
WHERE id = 1
RETURNING metadata_revision`,
		now-int64(revisionBumpInterval/time.Millisecond), now).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("bump metadata revision: %w", err)
	}
	return revision, nil
}

// PacerLease is one claimed slot on a shared upstream pacer.
type PacerLease struct {
	// AdmitAt is the unix-millisecond time the caller may issue its request.
	AdmitAt int64
	// LastUserAt is the last time live server traffic used this host. A job
	// uses it to tell whether the server is currently busy.
	LastUserAt int64
}
