package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func seedMetadataTrack(t *testing.T, database *sql.DB, name string) int64 {
	t.Helper()
	if _, err := database.ExecContext(context.Background(), `INSERT INTO tracks
(name, name_lower, artist_name, artist_name_lower, album_name, album_name_lower, duration)
VALUES (?, ?, 'Artist', 'artist', 'Album', 'album', 200)`, name, name); err != nil {
		t.Fatalf("seed track %q: %v", name, err)
	}
	var id int64
	if err := database.QueryRowContext(context.Background(),
		"SELECT id FROM tracks WHERE name_lower = ?", name).Scan(&id); err != nil {
		t.Fatalf("read track id: %v", err)
	}
	return id
}

func ageMetadataCheckedAt(t *testing.T, database *sql.DB, id int64, age time.Duration) {
	t.Helper()
	stamp := time.Now().Add(-age).UTC().Format("2006-01-02 15:04:05")
	if _, err := database.ExecContext(context.Background(),
		"UPDATE tracks SET metadata_checked = 1, metadata_checked_at = ? WHERE id = ?", stamp, id); err != nil {
		t.Fatalf("age track %d: %v", id, err)
	}
}

func TestListStaleTracksMetadataSelectsOnlyOldAnswers(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	old := seedMetadataTrack(t, metadataDB, "old")
	fresh := seedMetadataTrack(t, metadataDB, "fresh")
	unknown := seedMetadataTrack(t, metadataDB, "unknown")
	ageMetadataCheckedAt(t, metadataDB, old, 10*24*time.Hour)
	ageMetadataCheckedAt(t, metadataDB, fresh, time.Hour)
	// Settled before the column existed: the reading the column's own comment
	// says should be revisited, not treated as fresh.
	ageMetadataCheckedAt(t, metadataDB, unknown, 0)
	if _, err := metadataDB.ExecContext(ctx,
		"UPDATE tracks SET metadata_checked_at = NULL WHERE id = ?", unknown); err != nil {
		t.Fatalf("clear age: %v", err)
	}
	// Never-settled rows belong to the first phase.
	pending := seedMetadataTrack(t, metadataDB, "pending")

	page, err := ListStaleTracksMetadata(ctx, metadataDB, time.Now().Add(-72*time.Hour), "", 0, 10)
	if err != nil {
		t.Fatalf("ListStaleTracksMetadata: %v", err)
	}
	got := map[int64]bool{}
	for _, item := range page {
		got[item.ID] = true
	}
	if len(got) != 2 || !got[old] || !got[unknown] {
		t.Fatalf("stale set = %v, want the old answer and the unknown-age one", got)
	}
	if got[fresh] {
		t.Error("a freshly settled track must not be refreshed")
	}
	if got[pending] {
		t.Error("a never-settled track belongs to the first phase, not the refresh set")
	}
}

func TestListStaleTracksMetadataOrdersOldestFirst(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	unknown := seedMetadataTrack(t, metadataDB, "unknown")
	newer := seedMetadataTrack(t, metadataDB, "newer")
	older := seedMetadataTrack(t, metadataDB, "older")
	ageMetadataCheckedAt(t, metadataDB, newer, 5*24*time.Hour)
	ageMetadataCheckedAt(t, metadataDB, older, 9*24*time.Hour)
	ageMetadataCheckedAt(t, metadataDB, unknown, 0)
	if _, err := metadataDB.ExecContext(ctx,
		"UPDATE tracks SET metadata_checked_at = NULL WHERE id = ?", unknown); err != nil {
		t.Fatalf("clear age: %v", err)
	}

	page, err := ListStaleTracksMetadata(ctx, metadataDB, time.Now(), "", 0, 10)
	if err != nil {
		t.Fatalf("ListStaleTracksMetadata: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("got %d rows, want 3", len(page))
	}
	// "Most stale first" is the point of a refresh, and the unknown-age row is the
	// most stale of all by definition.
	if page[0].ID != unknown || page[1].ID != older || page[2].ID != newer {
		t.Fatalf("order = %d, %d, %d; want %d, %d, %d",
			page[0].ID, page[1].ID, page[2].ID, unknown, older, newer)
	}
	if page[0].CheckedAt != "" {
		t.Errorf("CheckedAt = %q, want empty for an unknown age", page[0].CheckedAt)
	}
}

// The tuple cursor is what makes a large refresh tractable: without it, a row that
// ties on age would either repeat or be skipped at every page boundary.
func TestListStaleTracksMetadataKeysetNeitherRepeatsNorSkips(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	var tied []int64
	for _, name := range []string{"t1", "t2", "t3"} {
		id := seedMetadataTrack(t, metadataDB, name)
		ageMetadataCheckedAt(t, metadataDB, id, 5*24*time.Hour)
		tied = append(tied, id)
	}
	older := seedMetadataTrack(t, metadataDB, "older")
	ageMetadataCheckedAt(t, metadataDB, older, 9*24*time.Hour)

	seen := map[int64]bool{}
	cursorAge, cursorID := "", int64(0)
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the scan did not terminate")
		}
		page, err := ListStaleTracksMetadata(ctx, metadataDB, time.Now(), cursorAge, cursorID, 2)
		if err != nil {
			t.Fatalf("ListStaleTracksMetadata: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			if seen[item.ID] {
				t.Fatalf("track %d was returned twice", item.ID)
			}
			seen[item.ID] = true
		}
		last := page[len(page)-1]
		cursorAge, cursorID = last.CheckedAt, last.ID
	}
	if len(seen) != 4 {
		t.Fatalf("saw %d rows, want all 4", len(seen))
	}
	for _, id := range append(tied, older) {
		if !seen[id] {
			t.Errorf("track %d was skipped", id)
		}
	}
}

func TestCountStaleTracksMetadataMatchesThePagedRows(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()
	for i, age := range []time.Duration{10 * 24 * time.Hour, 5 * 24 * time.Hour, time.Hour} {
		id := seedMetadataTrack(t, metadataDB, fmt.Sprintf("track-%d", i))
		ageMetadataCheckedAt(t, metadataDB, id, age)
	}
	seedMetadataTrack(t, metadataDB, "pending")

	cutoff := time.Now().Add(-72 * time.Hour)
	count, err := CountStaleTracksMetadata(ctx, metadataDB, cutoff)
	if err != nil {
		t.Fatalf("CountStaleTracksMetadata: %v", err)
	}
	page, err := ListStaleTracksMetadata(ctx, metadataDB, cutoff, "", 0, 100)
	if err != nil {
		t.Fatalf("ListStaleTracksMetadata: %v", err)
	}
	// The header number drives the lane plan, so it has to match what the producer
	// can actually hand out.
	if count != int64(len(page)) {
		t.Errorf("count = %d but the scan returned %d rows", count, len(page))
	}
	if count != 2 {
		t.Errorf("count = %d, want the two answers older than 3 days", count)
	}
}

func TestStaleMetadataQueriesRejectANilDatabase(t *testing.T) {
	ctx := context.Background()
	if _, err := ListStaleTracksMetadata(ctx, nil, time.Now(), "", 0, 10); err == nil {
		t.Error("ListStaleTracksMetadata(nil) should fail")
	}
	if _, err := CountStaleTracksMetadata(ctx, nil, time.Now()); err == nil {
		t.Error("CountStaleTracksMetadata(nil) should fail")
	}
}

// The settle column is stored as SQLite's CURRENT_TIMESTAMP text, so a cutoff
// rendered any other way would compare numerically and match everything or nothing.
func TestSQLTimeMatchesTheStoredFormat(t *testing.T) {
	stamp := sqlTime(time.Date(2024, 3, 9, 14, 5, 6, 0, time.UTC))
	if stamp != "2024-03-09 14:05:06" {
		t.Errorf("sqlTime = %q, want the CURRENT_TIMESTAMP layout", stamp)
	}
	// And it must normalize, because a local-time cutoff would be off by the
	// machine's offset.
	east := time.FixedZone("east", 9*3600)
	if got := sqlTime(time.Date(2024, 3, 9, 23, 5, 6, 0, east)); got != "2024-03-09 14:05:06" {
		t.Errorf("sqlTime = %q, want the UTC rendering", got)
	}
	// Lexicographic order over this fixed-width form is chronological, which is
	// what lets the stale scan page on it.
	if sqlTime(time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)) >=
		sqlTime(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("earlier timestamps must sort before later ones")
	}
}

// The default run's paging query must keep its existing index. A regression here
// is silent: the job still works, just gets slower the more of the library it has
// already resolved.
func TestPendingIndexStillServesTheDefaultRun(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	plan := queryPlan(t, metadataDB, `SELECT id FROM tracks
WHERE metadata_checked = 0 AND id > 0 ORDER BY id LIMIT 500`)
	if !usesIndex(plan, "idx_tracks_metadata_pending") {
		t.Errorf("default-run paging should use idx_tracks_metadata_pending, got %s", plan)
	}
}

// The refresh scan is the one that could fall back to a full table scan. On a large
// library that is the difference between a minutes-long job and an hours-long one,
// and nothing else in the run would show the difference.
func TestAgeIndexServesTheRefreshScan(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	for _, query := range []string{
		// The metadata refresh scan.
		`SELECT id FROM tracks
WHERE metadata_checked = 1 AND COALESCE(metadata_checked_at,'') < '2024-01-01 00:00:00'
AND (COALESCE(metadata_checked_at,'') > '' OR (COALESCE(metadata_checked_at,'') = '' AND id > 0))
ORDER BY COALESCE(metadata_checked_at,''), id LIMIT 500`,
		// The lyrics refresh scan, which must not be slower than its counterpart.
		`SELECT id FROM tracks
WHERE lyrics_checked = 1 AND COALESCE(lyrics_checked_at,'') < '2024-01-01 00:00:00'
AND (COALESCE(lyrics_checked_at,'') > '' OR (COALESCE(lyrics_checked_at,'') = '' AND id > 0))
ORDER BY COALESCE(lyrics_checked_at,''), id LIMIT 500`,
	} {
		plan := queryPlan(t, metadataDB, query)
		if !usesIndex(plan, "idx_tracks_metadata_age") && !usesIndex(plan, "idx_tracks_lyrics_age") {
			t.Errorf("refresh scan should use an age index, got %s", plan)
		}
		if containsScan(plan) {
			t.Errorf("refresh scan must not fall back to a full table scan, got %s", plan)
		}
	}
}

func TestLyricsPendingIndexServesTheFirstPhase(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	plan := queryPlan(t, metadataDB, `SELECT id FROM tracks
WHERE lyrics_checked = 0 AND id > 0 ORDER BY id LIMIT 500`)
	if !usesIndex(plan, "idx_tracks_lyrics_pending") {
		t.Errorf("lyrics first-phase paging should use idx_tracks_lyrics_pending, got %s", plan)
	}
	if containsScan(plan) {
		t.Errorf("lyrics first-phase paging must not full-scan, got %s", plan)
	}
}

// queryPlan returns SQLite's plan for a query as one string, so a test can assert
// on which index served it.
func queryPlan(t *testing.T, database *sql.DB, query string) string {
	t.Helper()
	rows, err := database.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		parts = append(parts, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if len(parts) == 0 {
		t.Fatalf("no query plan for %q", query)
	}
	return strings.Join(parts, " | ")
}

func usesIndex(plan, index string) bool { return strings.Contains(plan, index) }

// containsScan reports whether the plan walks the table rather than an index. A
// bare "SCAN" with no index name is the shape to catch.
func containsScan(plan string) bool {
	for _, part := range strings.Split(plan, "|") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "SCAN") && !strings.Contains(part, "USING") {
			return true
		}
	}
	return false
}
