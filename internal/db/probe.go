package db

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNilDatabase reports that a database operation was handed no handle.
var ErrNilDatabase = errors.New("database is nil")

// ProbeWritable verifies the database accepts a write.
//
// A BEGIN IMMEDIATE alone is not enough: SQLite acquires the write lock at BEGIN
// but defers the page write, so the statement succeeds against a read-only file
// and only the first real write reports SQLITE_READONLY. The probe therefore
// issues a genuine write inside a transaction and rolls it back, which exercises
// the same path a job batch takes.
//
// Inspecting the mode bits instead would be cheaper but wrong: POSIX ACLs and a
// setgid directory both change who may write, and root bypasses the check
// entirely. The rollback leaves no trace, and a crash mid-probe cannot leave one
// either, because SQLite discards an uncommitted transaction.
//
// This lives here rather than in the command layer so a job can probe a database
// other than the metadata one, and so every caller gets the same guarantee. The
// caller is responsible for turning the returned error into an operator-facing
// message, which needs the path and therefore belongs outside this package.
func ProbeWritable(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return ErrNilDatabase
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The table is never actually created: the rollback discards it. A name that
	// cannot collide with a real one keeps the probe harmless even if a future
	// change commits the transaction by mistake.
	_, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS job_write_probe (id INTEGER PRIMARY KEY)")
	return err
}
