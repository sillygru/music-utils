package db

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMetadataIndexListMatchesSchema keeps the two delivery paths for tracks
// indexes in step.
//
// metadata_schema.sql is applied wholesale only to a new database, and otherwise
// reaches an existing one solely through a search rebuild. ensureMetadataIndexes
// is what actually delivers an index to a settled deployment, so the two lists
// have to agree or an index added to the schema silently never arrives.
func TestMetadataIndexListMatchesSchema(t *testing.T) {
	schema, err := schemaFS.ReadFile("metadata_schema.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	declared := regexp.MustCompile(`(?im)^\s*CREATE INDEX IF NOT EXISTS (\w+) `).FindAllStringSubmatch(string(schema), -1)
	if len(declared) == 0 {
		t.Fatal("expected the schema to declare tracks indexes")
	}

	inSchema := make(map[string]bool, len(declared))
	for _, match := range declared {
		inSchema[match[1]] = true
	}
	inList := make(map[string]bool, len(metadataIndexes))
	for _, index := range metadataIndexes {
		// Entries read "<name> ON <table>(...) [WHERE ...]", so the name is the
		// first field.
		fields := strings.Fields(index)
		if len(fields) < 2 || fields[1] != "ON" {
			t.Errorf("malformed entry %q, want \"<name> ON <table>(...)\"", index)
			continue
		}
		inList[fields[0]] = true
	}

	for name := range inSchema {
		if !inList[name] {
			t.Errorf("index %q is in the schema but missing from metadataIndexes, so existing "+
				"deployments would never receive it", name)
		}
	}
	for name := range inList {
		if !inSchema[name] {
			t.Errorf("index %q is in metadataIndexes but not declared in the schema, so a new "+
				"database would not have it", name)
		}
	}
}

// TestMigrationDeliversPendingIndexToASettledDatabase is the case the two lists
// exist for: a database that is already fully migrated, with no column to add
// and a healthy search index, so nothing in the ordinary migration path would
// create a newly declared index.
func TestMigrationDeliversPendingIndexToASettledDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settled.db")
	database, err := Open(path, Config{MmapSize: 64 * 1024 * 1024, CacheSizeKB: -2000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := MigrateMetadata(ctx, database); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	// Stand in for a database created before this index existed, and which has
	// otherwise never triggered a schema change or search rebuild.
	if _, err := database.ExecContext(ctx, "DROP INDEX idx_tracks_metadata_pending"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	// The search index must be considered current, otherwise the rebuild would
	// create the index as a side effect and the test would prove nothing.
	if err := MigrateMetadata(ctx, database); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var count int
	if err := database.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_tracks_metadata_pending'").Scan(&count); err != nil {
		t.Fatalf("inspect index: %v", err)
	}
	if count != 1 {
		t.Error("migration did not create idx_tracks_metadata_pending on a settled database")
	}
}

// TestPendingIndexServesThePagingQuery pins the access path itself, not just the
// index's existence. A future schema change can leave the index in place while
// making it unusable, and the symptom would be a backfill that quietly gets
// slower as it succeeds.
func TestPendingIndexServesThePagingQuery(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	// Enough rows, and enough of them already checked, for the difference
	// between an index seek and a filtered range scan to show up in the plan.
	const total = 400
	for i := 0; i < total; i++ {
		checked := 0
		if i%10 != 0 {
			checked = 1
		}
		if _, err := metadataDB.ExecContext(ctx, `INSERT INTO tracks (name, name_lower, artist_name,
artist_name_lower, album_name, album_name_lower, duration, metadata_checked)
VALUES (?,?,?,?,?,?,?,?)`,
			"name", "name", "artist", "artist", "album", "album", float64(i), checked); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := metadataDB.ExecContext(ctx, "ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// The exact predicate ListTracksMissingMetadata uses, so this test fails if
	// the query changes shape and loses the index.
	rows, err := metadataDB.QueryContext(ctx, `EXPLAIN QUERY PLAN
SELECT id, name, artist_name, COALESCE(album_name, ''), COALESCE(duration, 0), created_at
FROM tracks WHERE metadata_checked = 0 AND id > ? ORDER BY id LIMIT ?`, 0, 500)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}

	planText := plan.String()
	if !strings.Contains(planText, "idx_tracks_metadata_pending") {
		t.Errorf("the pending-metadata query is not using idx_tracks_metadata_pending, so the "+
			"backfill will scan past every resolved row on each page.\nplan:\n%s", planText)
	}
	if strings.Contains(planText, "SCAN tracks") && !strings.Contains(planText, "COVERING INDEX") {
		t.Errorf("expected a covering index search, got a table scan.\nplan:\n%s", planText)
	}
}
