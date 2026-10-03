package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// ageCheckedAt backdates the settle timestamp of a track by the given age, so a
// test can place a row on either side of a cutoff without sleeping.
func ageLyricsCheckedAt(t *testing.T, database *sql.DB, id int64, age time.Duration) {
	t.Helper()
	stamp := time.Now().Add(-age).UTC().Format("2006-01-02 15:04:05")
	if _, err := database.ExecContext(context.Background(),
		"UPDATE tracks SET lyrics_checked = 1, lyrics_checked_at = ? WHERE id = ?", stamp, id); err != nil {
		t.Fatalf("age track %d: %v", id, err)
	}
}

func seedLyricsTrack(t *testing.T, database *sql.DB, name string) int64 {
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

func TestListTracksMissingLyricsReturnsOnlyUnsettled(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	pending := seedLyricsTrack(t, metadataDB, "pending")
	settled := seedLyricsTrack(t, metadataDB, "settled")
	ageLyricsCheckedAt(t, metadataDB, settled, time.Hour)

	page, err := ListTracksMissingLyrics(ctx, metadataDB, 0, 10)
	if err != nil {
		t.Fatalf("ListTracksMissingLyrics: %v", err)
	}
	if len(page) != 1 || page[0].ID != pending {
		t.Fatalf("got %+v, want only the unsettled track %d", page, pending)
	}
	if page[0].Artist != "Artist" || page[0].Album != "Album" {
		t.Errorf("work is missing query fields: %+v", page[0])
	}
	if page[0].ISRC != "" {
		t.Errorf("ISRC = %q, want empty", page[0].ISRC)
	}
}

func TestListTracksMissingLyricsCarriesISRC(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()
	if _, err := metadataDB.ExecContext(ctx, `INSERT INTO tracks
(name, name_lower, artist_name, artist_name_lower, isrc, duration)
VALUES ('Song', 'song', 'Artist', 'artist', 'USRC12345678', 210)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	page, err := ListTracksMissingLyrics(ctx, metadataDB, 0, 10)
	if err != nil {
		t.Fatalf("ListTracksMissingLyrics: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("got %d rows, want 1", len(page))
	}
	// LyricsPlus matches on ISRC, so dropping it would silently weaken lookups.
	if page[0].ISRC != "USRC12345678" {
		t.Errorf("ISRC = %q, want USRC12345678", page[0].ISRC)
	}
	if page[0].Duration != 210 {
		t.Errorf("Duration = %v, want 210", page[0].Duration)
	}
}

func TestListTracksMissingLyricsPagesByID(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()
	first := seedLyricsTrack(t, metadataDB, "a")
	second := seedLyricsTrack(t, metadataDB, "b")
	seedLyricsTrack(t, metadataDB, "c")

	page, err := ListTracksMissingLyrics(ctx, metadataDB, 0, 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(page) != 2 || page[0].ID != first || page[1].ID != second {
		t.Fatalf("first page = %+v", page)
	}
	page, err = ListTracksMissingLyrics(ctx, metadataDB, page[1].ID, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(page) != 1 || page[0].ID <= second {
		t.Fatalf("second page = %+v, want the row after %d", page, second)
	}
}

func TestListStaleLyricsSelectsOnlyOldAnswers(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	old := seedLyricsTrack(t, metadataDB, "old")
	fresh := seedLyricsTrack(t, metadataDB, "fresh")
	unknownAge := seedLyricsTrack(t, metadataDB, "unknown")
	ageLyricsCheckedAt(t, metadataDB, old, 10*24*time.Hour)
	ageLyricsCheckedAt(t, metadataDB, fresh, time.Hour)
	// Settled with no recorded time: the pre-upgrade category, which must be
	// treated as maximally stale rather than skipped.
	ageLyricsCheckedAt(t, metadataDB, unknownAge, 0)
	if _, err := metadataDB.ExecContext(ctx,
		"UPDATE tracks SET lyrics_checked_at = NULL WHERE id = ?", unknownAge); err != nil {
		t.Fatalf("clear age: %v", err)
	}
	// A never-settled track is the first phase's job, not the refresh set's.
	pending := seedLyricsTrack(t, metadataDB, "pending")

	page, err := ListStaleLyrics(ctx, metadataDB, time.Now().Add(-72*time.Hour), "", 0, 10)
	if err != nil {
		t.Fatalf("ListStaleLyrics: %v", err)
	}
	got := map[int64]bool{}
	for _, item := range page {
		got[item.ID] = true
	}
	if len(got) != 2 || !got[old] || !got[unknownAge] {
		t.Fatalf("stale set = %v, want the old answer and the unknown-age one", got)
	}
	if got[fresh] {
		t.Error("a freshly settled track must not be refreshed")
	}
	if got[pending] {
		t.Error("a never-settled track belongs to the first phase, not the refresh set")
	}
}

// A row with no recorded settle time must sort before every real timestamp, so a
// refresh reaches the unknown-age rows first instead of last.
func TestListStaleLyricsOrdersUnknownAgeFirst(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	known := seedLyricsTrack(t, metadataDB, "known")
	unknown := seedLyricsTrack(t, metadataDB, "unknown")
	ageLyricsCheckedAt(t, metadataDB, known, 10*24*time.Hour)
	ageLyricsCheckedAt(t, metadataDB, unknown, 0)
	if _, err := metadataDB.ExecContext(ctx,
		"UPDATE tracks SET lyrics_checked_at = NULL WHERE id = ?", unknown); err != nil {
		t.Fatalf("clear age: %v", err)
	}

	page, err := ListStaleLyrics(ctx, metadataDB, time.Now(), "", 0, 10)
	if err != nil {
		t.Fatalf("ListStaleLyrics: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("got %d rows, want 2", len(page))
	}
	if page[0].ID != unknown || page[1].ID != known {
		t.Fatalf("order = %d, %d; want the unknown-age row %d first", page[0].ID, page[1].ID, unknown)
	}
	if page[0].CheckedAt != "" {
		t.Errorf("CheckedAt = %q, want empty for an unknown age", page[0].CheckedAt)
	}
}

// The stale scan pages on (age, id), so the cursor has to resume mid-scan without
// repeating or skipping a row that ties on age.
func TestListStaleLyricsKeysetNeitherRepeatsNorSkips(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	// Three rows sharing one settle time, so the id tiebreak does the work.
	var tied []int64
	for _, name := range []string{"t1", "t2", "t3"} {
		id := seedLyricsTrack(t, metadataDB, name)
		ageLyricsCheckedAt(t, metadataDB, id, 5*24*time.Hour)
		tied = append(tied, id)
	}
	older := seedLyricsTrack(t, metadataDB, "older")
	ageLyricsCheckedAt(t, metadataDB, older, 9*24*time.Hour)

	seen := map[int64]bool{}
	cursorAge, cursorID := "", int64(0)
	for {
		page, err := ListStaleLyrics(ctx, metadataDB, time.Now(), cursorAge, cursorID, 2)
		if err != nil {
			t.Fatalf("ListStaleLyrics: %v", err)
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
		t.Fatalf("saw %d rows (%v), want all 4", len(seen), seen)
	}
	for _, id := range append(tied, older) {
		if !seen[id] {
			t.Errorf("track %d was skipped", id)
		}
	}
}

func TestCountStaleLyricsMatchesThePagedRows(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()
	for i, age := range []time.Duration{10 * 24 * time.Hour, 5 * 24 * time.Hour, time.Hour} {
		id := seedLyricsTrack(t, metadataDB, fmt.Sprintf("track-%d", i))
		ageLyricsCheckedAt(t, metadataDB, id, age)
	}
	seedLyricsTrack(t, metadataDB, "pending")

	cutoff := time.Now().Add(-72 * time.Hour)
	count, err := CountStaleLyrics(ctx, metadataDB, cutoff)
	if err != nil {
		t.Fatalf("CountStaleLyrics: %v", err)
	}
	// The header number drives the lane plan, so it has to match what the producer
	// can actually hand out.
	page, err := ListStaleLyrics(ctx, metadataDB, cutoff, "", 0, 100)
	if err != nil {
		t.Fatalf("ListStaleLyrics: %v", err)
	}
	if count != int64(len(page)) {
		t.Errorf("count = %d but the scan returned %d rows", count, len(page))
	}
	if count != 2 {
		t.Errorf("count = %d, want the two answers older than 3 days", count)
	}
}

func TestCountTracksMissingLyricsIgnoresSettledRows(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()
	seedLyricsTrack(t, metadataDB, "a")
	settled := seedLyricsTrack(t, metadataDB, "b")
	ageLyricsCheckedAt(t, metadataDB, settled, time.Hour)

	count, err := CountTracksMissingLyrics(ctx, metadataDB)
	if err != nil {
		t.Fatalf("CountTracksMissingLyrics: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

// The per-page filter that keeps a first run from re-fetching a library that is
// already whole.
func TestTrackIDsWithUsableLyricsFindsEveryFormOfAnswer(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()

	plain := seedLyricsTrack(t, metadataDB, "plain")
	synced := seedLyricsTrack(t, metadataDB, "synced")
	instrumental := seedLyricsTrack(t, metadataDB, "instrumental")
	none := seedLyricsTrack(t, metadataDB, "none")

	// The seeded rows all share one identity, so each insert has to reuse that
	// identity rather than invent one: the upsert is keyed on the unique tuple, not
	// on the id, and a new duration would insert a second row instead of filling
	// the track under test.
	identity := func(name string) Track {
		return Track{Name: name, NameLower: name, ArtistName: "Artist", ArtistNameLower: "artist",
			AlbumName: "Album", AlbumNameLower: "album", Duration: 200}
	}
	if _, _, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, identity("plain"), Lyrics{PlainLyrics: "words"}); err != nil {
		t.Fatalf("seed plain: %v", err)
	}
	if _, _, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, identity("synced"), Lyrics{SyncedLyrics: "[00:01.00]la"}); err != nil {
		t.Fatalf("seed synced: %v", err)
	}
	// An instrumental answer is a real answer, so it has to count as one.
	if _, _, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, identity("instrumental"), Lyrics{Instrumental: true}); err != nil {
		t.Fatalf("seed instrumental: %v", err)
	}
	ids := []int64{plain, synced, instrumental, none, 0, -1}
	found, err := TrackIDsWithUsableLyrics(ctx, lyricsDB, ids)
	if err != nil {
		t.Fatalf("TrackIDsWithUsableLyrics: %v", err)
	}
	for _, id := range []int64{plain, synced, instrumental} {
		if !found[id] {
			t.Errorf("track %d should count as having usable lyrics", id)
		}
	}
	if found[none] {
		t.Error("a track with no lyrics row must not count as usable")
	}
	if found[0] || found[-1] {
		t.Error("non-positive ids must be ignored")
	}
}

func TestTrackIDsWithUsableLyricsHandlesEmptyInput(t *testing.T) {
	_, lyricsDB := testDatabases(t)
	ctx := context.Background()
	for _, ids := range [][]int64{nil, {}, {0, -1}} {
		found, err := TrackIDsWithUsableLyrics(ctx, lyricsDB, ids)
		if err != nil {
			t.Fatalf("ids %v: %v", ids, err)
		}
		if len(found) != 0 {
			t.Errorf("ids %v: found %v, want empty", ids, found)
		}
	}
	// A nil database must not panic: the filter is an optimization, so a missing
	// lyrics store degrades to asking upstream rather than failing the run.
	found, err := TrackIDsWithUsableLyrics(ctx, nil, []int64{1})
	if err != nil {
		t.Fatalf("nil database: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("nil database: found %v, want empty", found)
	}
}

func TestMarkTrackLyricsCheckedStampsBothColumns(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()
	id := seedLyricsTrack(t, metadataDB, "song")

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := MarkTrackLyricsChecked(ctx, tx, id); err != nil {
		t.Fatalf("MarkTrackLyricsChecked: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var checked int
	var at sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT lyrics_checked, lyrics_checked_at FROM tracks WHERE id = ?", id).
		Scan(&checked, &at); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if checked != 1 {
		t.Errorf("lyrics_checked = %d, want 1", checked)
	}
	if !at.Valid || at.String == "" {
		t.Error("a definitive answer must record when it was given")
	}
}

// The distinction that keeps a first run from re-fetching an already whole
// library, while still leaving those rows for a later refresh.
func TestMarkTrackLyricsSettledUnknownAgeLeavesNoTimestamp(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()
	id := seedLyricsTrack(t, metadataDB, "song")

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := MarkTrackLyricsSettledUnknownAge(ctx, tx, id); err != nil {
		t.Fatalf("MarkTrackLyricsSettledUnknownAge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var checked int
	var at sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT lyrics_checked, lyrics_checked_at FROM tracks WHERE id = ?", id).
		Scan(&checked, &at); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if checked != 1 {
		t.Errorf("lyrics_checked = %d, want 1", checked)
	}
	// Inventing a time here would make the whole existing library look freshly
	// settled, so the first refresh would skip all of it.
	if at.Valid {
		t.Errorf("lyrics_checked_at = %q, want NULL", at.String)
	}
	// And a NULL age must still be reachable by a refresh.
	page, err := ListStaleLyrics(ctx, metadataDB, time.Now(), "", 0, 10)
	if err != nil {
		t.Fatalf("ListStaleLyrics: %v", err)
	}
	if len(page) != 1 || page[0].ID != id {
		t.Errorf("an unknown-age row should be refreshable, got %+v", page)
	}
}

func TestMarkTrackLyricsStampsRequireATransaction(t *testing.T) {
	_, _ = testDatabases(t)
	ctx := context.Background()
	if err := MarkTrackLyricsChecked(ctx, nil, 1); err == nil {
		t.Error("MarkTrackLyricsChecked(nil tx) should fail")
	}
	if err := MarkTrackLyricsSettledUnknownAge(ctx, nil, 1); err == nil {
		t.Error("MarkTrackLyricsSettledUnknownAge(nil tx) should fail")
	}
}

func TestLyricsSelectionQueriesRejectANilDatabase(t *testing.T) {
	ctx := context.Background()
	if _, err := ListTracksMissingLyrics(ctx, nil, 0, 10); err == nil {
		t.Error("ListTracksMissingLyrics(nil) should fail")
	}
	if _, err := ListStaleLyrics(ctx, nil, time.Now(), "", 0, 10); err == nil {
		t.Error("ListStaleLyrics(nil) should fail")
	}
	if _, err := CountTracksMissingLyrics(ctx, nil); err == nil {
		t.Error("CountTracksMissingLyrics(nil) should fail")
	}
	if _, err := CountStaleLyrics(ctx, nil, time.Now()); err == nil {
		t.Error("CountStaleLyrics(nil) should fail")
	}
}

// A lyrics row that exists but carries nothing is the shape the live path
// deliberately treats as a cache miss, and the backfill must agree: counting it as
// an answer would re-ask the provider on every single run forever.
//
// This cannot be folded into the test above, because the lyrics store is
// content-addressed. An empty insert resolves to whichever row already holds the
// empty-content hash, so an instrumental track elsewhere in the test would silently
// lend this one a usable row.
func TestTrackIDsWithUsableLyricsRejectsAnEmptyLyricsRow(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()

	empty := seedLyricsTrack(t, metadataDB, "empty")
	if _, _, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB,
		Track{Name: "empty", NameLower: "empty", ArtistName: "Artist", ArtistNameLower: "artist",
			AlbumName: "Album", AlbumNameLower: "album", Duration: 200},
		Lyrics{Source: "empty"}); err != nil {
		t.Fatalf("seed empty: %v", err)
	}

	found, err := TrackIDsWithUsableLyrics(ctx, lyricsDB, []int64{empty})
	if err != nil {
		t.Fatalf("TrackIDsWithUsableLyrics: %v", err)
	}
	if found[empty] {
		t.Error("an empty lyrics row must not count as usable")
	}
}
