package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
)

//go:embed metadata_schema.sql lyrics_schema.sql lyrics_sync_schema.sql covers_schema.sql
var schemaFS embed.FS

var metadataColumns = []struct {
	name       string
	definition string
}{
	{"genre", "TEXT"},
	{"genre_lower", "TEXT"},
	{"year", "INTEGER"},
	{"release_date", "TEXT"},
	{"isrc", "TEXT"},
	{"musicbrainz_recording_id", "TEXT"},
	{"musicbrainz_release_id", "TEXT"},
	{"musicbrainz_release_group_id", "TEXT"},
	{"musicbrainz_artist_id", "TEXT"},
	{"cover_url", "TEXT"},
	{"metadata_source", "TEXT"},
	{"cover_url_source", "TEXT"},
	{"metadata_checked", "BOOLEAN NOT NULL DEFAULT 0"},
	{"cover_url_checked", "BOOLEAN NOT NULL DEFAULT 0"},
}

// MigrateMetadata initializes the metadata database and upgrades the former
// combined database without leaving a foreign key to the lyrics database.
func MigrateMetadata(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return fmt.Errorf("database is nil")
	}
	schema, err := schemaFS.ReadFile("metadata_schema.sql")
	if err != nil {
		return fmt.Errorf("read metadata schema: %w", err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metadata migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var tracksExists int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='tracks'").Scan(&tracksExists); err != nil {
		return fmt.Errorf("inspect tracks table: %w", err)
	}
	if tracksExists == 0 {
		if _, err = tx.ExecContext(ctx, string(schema)); err != nil {
			return fmt.Errorf("apply metadata schema: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit metadata migration: %w", err)
		}
		return nil
	}

	legacyReference, err := tracksReferencesLegacyLyrics(ctx, tx)
	if err != nil {
		return fmt.Errorf("inspect legacy track foreign keys: %w", err)
	}
	if legacyReference {
		if err = rebuildLegacyTracks(ctx, tx, schema); err != nil {
			return fmt.Errorf("rebuild legacy tracks: %w", err)
		}
	} else {
		schemaChanged := false
		for _, column := range metadataColumns {
			var count int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('tracks') WHERE name = ?", column.name).Scan(&count); err != nil {
				return fmt.Errorf("inspect tracks column %s: %w", column.name, err)
			}
			if count == 0 {
				if _, err = tx.ExecContext(ctx, "ALTER TABLE tracks ADD COLUMN "+column.name+" "+column.definition); err != nil {
					return fmt.Errorf("add tracks column %s: %w", column.name, err)
				}
				schemaChanged = true
			}
		}
		needsSearchRefresh, refreshErr := metadataSearchNeedsRefresh(ctx, tx)
		if refreshErr != nil {
			return fmt.Errorf("inspect metadata search: %w", refreshErr)
		}
		if schemaChanged || needsSearchRefresh {
			if err = refreshMetadataSearch(ctx, tx, schema); err != nil {
				return fmt.Errorf("refresh metadata search: %w", err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit metadata migration: %w", err)
	}
	return nil
}

// MigrateLyrics initializes the independent lyrics database, including the
// additive rich/word-synchronized variants table.
func MigrateLyrics(ctx context.Context, database *sql.DB) error {
	if err := migrateSchema(ctx, database, "lyrics_schema.sql"); err != nil {
		return err
	}
	return migrateSchema(ctx, database, "lyrics_sync_schema.sql")
}

// MigrateCover initializes the independent album/artist cover URL database and
// backfills variant rows for cover rows that predate the variants table.
func MigrateCover(ctx context.Context, database *sql.DB) error {
	if err := migrateSchema(ctx, database, "covers_schema.sql"); err != nil {
		return err
	}
	return backfillCoverVariants(ctx, database)
}

// backfillCoverVariants seeds a rank-0 variant row for every positive cover
// row created before the variants table existed, so existing deployments keep
// serving a complete results list after upgrading. Idempotent: rows that
// already have any variant are left alone, so re-running the migration is a
// no-op.
func backfillCoverVariants(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return fmt.Errorf("database is nil")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cover variant backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO cover_url_variants (cover_url_id, url, source, rank)
SELECT id, cover_url, cover_source, 0 FROM cover_urls cu
WHERE cover_url IS NOT NULL AND cover_url <> ''
AND NOT EXISTS (SELECT 1 FROM cover_url_variants v WHERE v.cover_url_id = cu.id)`); err != nil {
		return fmt.Errorf("backfill cover variants: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit cover variant backfill: %w", err)
	}
	return nil
}

// Migrate remains a metadata-only convenience for package callers that only
// need to initialize a metadata database.
func Migrate(ctx context.Context, database *sql.DB) error { return MigrateMetadata(ctx, database) }

func migrateSchema(ctx context.Context, database *sql.DB, filename string) error {
	if database == nil {
		return fmt.Errorf("database is nil")
	}
	schema, err := schemaFS.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read %s: %w", filename, err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s migration: %w", filename, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, string(schema)); err != nil {
		return fmt.Errorf("apply %s: %w", filename, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit %s migration: %w", filename, err)
	}
	return nil
}

func tracksReferencesLegacyLyrics(ctx context.Context, tx *sql.Tx) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_list(tracks)")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, sequence int
		var tableName, from, to, onUpdate, onDelete, match sql.NullString
		if err := rows.Scan(&id, &sequence, &tableName, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return false, err
		}
		if tableName.Valid && strings.EqualFold(tableName.String, "lyrics") && from.Valid && from.String == "last_lyrics_id" {
			return true, nil
		}
	}
	return false, rows.Err()
}

// rebuildLegacyTracks creates the new metadata table without the old
// tracks->lyrics foreign key. tracks_legacy is intentionally retained until
// MigrateLegacyLyrics removes the old lyrics tables in dependency order.
func rebuildLegacyTracks(ctx context.Context, tx *sql.Tx, schema []byte) error {
	for _, statement := range []string{
		"DROP TRIGGER IF EXISTS tracks_ai",
		"DROP TRIGGER IF EXISTS tracks_ad",
		"DROP TRIGGER IF EXISTS tracks_au",
		"DROP TABLE IF EXISTS tracks_fts",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("remove legacy track search objects: %w", err)
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='tracks' AND name NOT LIKE 'sqlite_autoindex_%'")
	if err != nil {
		return fmt.Errorf("inspect legacy track indexes: %w", err)
	}
	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy track index: %w", err)
		}
		indexes = append(indexes, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read legacy track indexes: %w", err)
	}
	_ = rows.Close()
	for _, name := range indexes {
		if _, err := tx.ExecContext(ctx, "DROP INDEX "+quoteIdentifier(name)); err != nil {
			return fmt.Errorf("drop legacy track index %s: %w", name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE tracks RENAME TO tracks_legacy"); err != nil {
		return fmt.Errorf("rename legacy tracks table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, string(schema)); err != nil {
		return fmt.Errorf("create rebuilt metadata schema: %w", err)
	}

	available, err := tableColumns(ctx, tx, "tracks_legacy")
	if err != nil {
		return fmt.Errorf("inspect legacy track columns: %w", err)
	}
	allColumns := []string{"id", "name", "name_lower", "artist_name", "artist_name_lower", "album_name", "album_name_lower", "duration", "genre", "genre_lower", "year", "release_date", "isrc", "musicbrainz_recording_id", "musicbrainz_release_id", "musicbrainz_release_group_id", "musicbrainz_artist_id", "cover_url", "metadata_source", "cover_url_source", "metadata_checked", "cover_url_checked", "last_lyrics_id", "source", "created_at", "updated_at"}
	requiredColumns := []string{"id", "name", "name_lower", "artist_name", "artist_name_lower"}
	for _, column := range requiredColumns {
		if !available[column] {
			return fmt.Errorf("legacy tracks table is missing required column %s", column)
		}
	}
	columns := make([]string, 0, len(allColumns))
	for _, column := range allColumns {
		if available[column] {
			columns = append(columns, column)
		}
	}
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdentifier(column)
	}
	statement := "INSERT INTO tracks (" + strings.Join(quoted, ",") + ") SELECT " + strings.Join(quoted, ",") + " FROM tracks_legacy"
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("copy legacy tracks: %w", err)
	}
	return nil
}

func metadataSearchNeedsRefresh(ctx context.Context, tx *sql.Tx) (bool, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='tracks_fts'").Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return true, nil
	}
	columns, err := tableColumns(ctx, tx, "tracks_fts")
	if err != nil {
		return false, err
	}
	return !columns["genre_lower"], nil
}

func refreshMetadataSearch(ctx context.Context, tx *sql.Tx, schema []byte) error {
	for _, statement := range []string{
		"DROP TRIGGER IF EXISTS tracks_ai",
		"DROP TRIGGER IF EXISTS tracks_ad",
		"DROP TRIGGER IF EXISTS tracks_au",
		"DROP TABLE IF EXISTS tracks_fts",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, string(schema)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO tracks_fts(tracks_fts) VALUES ('rebuild')")
	return err
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func tableColumns(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) (map[string]bool, error) {
	rows, err := queryer.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

// MigrateLegacyLyrics copies lyrics out of an old combined metadata database.
// It is idempotent and removes old tables only after all data is committed.
func MigrateLegacyLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB) error {
	if metadataDB == nil || lyricsDB == nil {
		return fmt.Errorf("metadata and lyrics databases are required")
	}
	metadataConn, err := metadataDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve metadata migration connection: %w", err)
	}
	defer metadataConn.Close()
	// The old combined schema has a cycle: lyrics references tracks and tracks
	// references lyrics. SQLite cannot drop either side while FK enforcement is
	// enabled, so disable it on this dedicated connection for this controlled,
	// one-time table removal only.
	if _, err = metadataConn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("disable legacy foreign keys: %w", err)
	}
	defer func() { _, _ = metadataConn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON") }()

	var exists int
	if err = metadataConn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='lyrics'").Scan(&exists); err != nil {
		return fmt.Errorf("inspect legacy lyrics table: %w", err)
	}
	if exists == 0 {
		_, _ = metadataConn.ExecContext(ctx, "DROP TABLE IF EXISTS tracks_legacy")
		return nil
	}

	lyricsTx, err := lyricsDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy lyrics migration: %w", err)
	}
	defer func() { _ = lyricsTx.Rollback() }()
	available, err := tableColumns(ctx, metadataConn, "lyrics")
	if err != nil {
		return fmt.Errorf("inspect legacy lyrics columns: %w", err)
	}
	if !available["id"] {
		return fmt.Errorf("legacy lyrics table is missing required column id")
	}
	columnExpression := func(column, fallback string) string {
		if available[column] {
			return quoteIdentifier(column)
		}
		return fallback + " AS " + quoteIdentifier(column)
	}
	selectStatement := "SELECT " + strings.Join([]string{
		columnExpression("id", "0"), columnExpression("track_id", "0"), columnExpression("plain_lyrics", "NULL"),
		columnExpression("synced_lyrics", "NULL"), columnExpression("has_plain_lyrics", "0"), columnExpression("has_synced_lyrics", "0"),
		columnExpression("instrumental", "0"), columnExpression("content_hash", "NULL"), columnExpression("source", "'local'"),
		columnExpression("created_at", "CURRENT_TIMESTAMP"),
	}, ",") + " FROM lyrics ORDER BY id"
	rows, err := metadataConn.QueryContext(ctx, selectStatement)
	if err != nil {
		return fmt.Errorf("read legacy lyrics: %w", err)
	}
	for rows.Next() {
		var id, trackID int64
		var plain, synced, hash, source, createdAt sql.NullString
		var hasPlain, hasSynced, instrumental bool
		if err = rows.Scan(&id, &trackID, &plain, &synced, &hasPlain, &hasSynced, &instrumental, &hash, &source, &createdAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy lyrics: %w", err)
		}
		if !hash.Valid || hash.String == "" {
			hash = sql.NullString{String: contentHash(plain.String, synced.String), Valid: true}
		}
		if _, err = lyricsTx.ExecContext(ctx, `INSERT OR IGNORE INTO lyrics (id,track_id,plain_lyrics,synced_lyrics,has_plain_lyrics,has_synced_lyrics,instrumental,content_hash,source,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, id, trackID, plain, synced, hasPlain, hasSynced, instrumental, hash, source, createdAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("copy legacy lyrics: %w", err)
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read legacy lyrics: %w", err)
	}
	_ = rows.Close()

	var associationExists int
	if err = metadataConn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='lyrics_tracks'").Scan(&associationExists); err != nil {
		return fmt.Errorf("inspect legacy lyrics associations: %w", err)
	}
	if associationExists > 0 {
		associationRows, queryErr := metadataConn.QueryContext(ctx, `SELECT track_id, lyrics_id FROM lyrics_tracks`)
		if queryErr != nil {
			return fmt.Errorf("read legacy lyrics associations: %w", queryErr)
		}
		for associationRows.Next() {
			var trackID, lyricsID int64
			if err = associationRows.Scan(&trackID, &lyricsID); err != nil {
				_ = associationRows.Close()
				return fmt.Errorf("scan legacy lyrics association: %w", err)
			}
			if _, err = lyricsTx.ExecContext(ctx, `INSERT OR IGNORE INTO lyrics_tracks (track_id,lyrics_id) VALUES (?,?)`, trackID, lyricsID); err != nil {
				_ = associationRows.Close()
				return fmt.Errorf("copy legacy lyrics association: %w", err)
			}
		}
		if err = associationRows.Err(); err != nil {
			_ = associationRows.Close()
			return fmt.Errorf("read legacy lyrics associations: %w", err)
		}
		_ = associationRows.Close()
	}
	if err = lyricsTx.Commit(); err != nil {
		return fmt.Errorf("commit legacy lyrics migration: %w", err)
	}

	// Drop children before parents to satisfy the old combined schema's FKs.
	cleanupTx, err := metadataConn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy table cleanup: %w", err)
	}
	for _, table := range []string{"lyrics_tracks", "lyrics", "tracks_legacy"} {
		if _, err = cleanupTx.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			_ = cleanupTx.Rollback()
			return fmt.Errorf("remove legacy %s table: %w", table, err)
		}
	}
	if err = cleanupTx.Commit(); err != nil {
		return fmt.Errorf("commit legacy table cleanup: %w", err)
	}
	return nil
}
