package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestListAndCountTracksMissingMetadata(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	// Two tracks resolved upstream, one never checked.
	resolved, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Creep", ArtistName: "Radiohead", AlbumName: "Pablo Honey",
		Duration: 238, Genre: "Rock", MetadataSource: "itunes",
		MetadataChecked: true, CoverURLChecked: true, Source: "itunes",
	})
	if err != nil {
		t.Fatalf("upsert resolved track: %v", err)
	}
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "No Surprises", ArtistName: "Radiohead", AlbumName: "OK Computer",
		Duration: 229, Genre: "Rock", MetadataSource: "itunes",
		MetadataChecked: true, CoverURLChecked: true, Source: "itunes",
	}); err != nil {
		t.Fatalf("upsert second resolved track: %v", err)
	}
	unchecked, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Shallow", ArtistName: "Lady Gaga", AlbumName: "A Star Is Born",
		Duration: 215, MetadataSource: "lrclib_fallback", Source: "lrclib_fallback",
	})
	if err != nil {
		t.Fatalf("upsert unchecked track: %v", err)
	}
	if unchecked == resolved {
		t.Fatal("expected distinct track ids")
	}

	count, err := CountTracksMissingMetadata(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count missing metadata: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 track missing metadata, got %d", count)
	}

	work, err := ListTracksMissingMetadata(ctx, metadataDB, 0, 100)
	if err != nil {
		t.Fatalf("list missing metadata: %v", err)
	}
	if len(work) != 1 {
		t.Fatalf("expected 1 work item, got %d", len(work))
	}
	if work[0].ID != unchecked {
		t.Fatalf("expected work for track %d, got %d", unchecked, work[0].ID)
	}
	if work[0].Name != "Shallow" || work[0].Artist != "Lady Gaga" {
		t.Fatalf("unexpected work item: %+v", work[0])
	}

	// Paging from an ID must skip rows already returned.
	next, err := ListTracksMissingMetadata(ctx, metadataDB, work[0].ID, 100)
	if err != nil {
		t.Fatalf("list next page: %v", err)
	}
	if len(next) != 0 {
		t.Fatalf("expected no further work items, got %d", len(next))
	}
}

func TestListTracksMissingMetadataHonorsLimit(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	for _, name := range []string{"A", "B", "C"} {
		if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
			Name: name, ArtistName: "Artist", AlbumName: "Album",
			Duration: 200, Source: "lrclib_fallback",
		}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}

	work, err := ListTracksMissingMetadata(ctx, metadataDB, 0, 2)
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(work) != 2 {
		t.Fatalf("expected limit of 2, got %d", len(work))
	}
}

func TestMergeTrackMetadataFillsGapsWithoutClobbering(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	// A track created by a lyrics lookup: name and artist known, album empty,
	// no genre, no year, and metadata_source names the lyrics provider.
	id, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Shallow", ArtistName: "Lady Gaga", Duration: 215,
		MetadataSource: "lrclib_fallback", Source: "lrclib_fallback",
	})
	if err != nil {
		t.Fatalf("upsert track: %v", err)
	}

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// The provider returns a different album variant than the row would key on,
	// plus a genre and year, and no cover URL.
	if err := MergeTrackMetadata(ctx, tx, id, Track{
		Name: "Shallow", ArtistName: "Lady Gaga", AlbumName: "A Star Is Born",
		Duration: 216, Genre: "Soundtrack", GenreLower: "soundtrack",
		Year: 2018, MetadataSource: "itunes", MetadataChecked: true,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("merge metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var (
		name, artist, album, genre, source string
		duration                           float64
		year                               int
		checked                            bool
	)
	row := metadataDB.QueryRowContext(ctx,
		`SELECT name, artist_name, COALESCE(album_name,''), COALESCE(genre,''),
duration, year, COALESCE(metadata_source,''), metadata_checked
FROM tracks WHERE id = ?`, id)
	if err := row.Scan(&name, &artist, &album, &genre, &duration, &year, &source, &checked); err != nil {
		t.Fatalf("read merged track: %v", err)
	}
	if album != "A Star Is Born" {
		t.Errorf("expected missing album to be filled, got %q", album)
	}
	if genre != "Soundtrack" {
		t.Errorf("expected genre Soundtrack, got %q", genre)
	}
	if year != 2018 {
		t.Errorf("expected year 2018, got %d", year)
	}
	if !checked {
		t.Error("expected metadata_checked to be set")
	}
	// Provenance now reflects the provider that actually supplied the metadata.
	if source != "itunes" {
		t.Errorf("expected metadata_source itunes, got %q", source)
	}
	// The row's identity must survive: it is still the same song, and the
	// existing duration is not overwritten by a near-duplicate variant.
	if name != "Shallow" || artist != "Lady Gaga" {
		t.Errorf("identity changed: %q / %q", name, artist)
	}
	if duration != 215 {
		t.Errorf("expected stored duration 215 to be preserved, got %v", duration)
	}
}

func TestMergeTrackMetadataSkipsIdentityChangeThatWouldDuplicate(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	// The pending row has an incomplete identity. Enriching it with the
	// provider's album and duration would collide with this already-cached row.
	pendingID, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", Source: "lrclib_fallback",
	})
	if err != nil {
		t.Fatalf("upsert pending track: %v", err)
	}
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 200,
		Source: "itunes", MetadataSource: "itunes",
	}); err != nil {
		t.Fatalf("upsert existing identity: %v", err)
	}

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MergeTrackMetadata(ctx, tx, pendingID, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 200,
		Genre: "Rock", Year: 2020, MetadataSource: "deezer", MetadataChecked: true,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("merge metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var album, genre, source string
	var duration float64
	var year int
	var checked bool
	if err := metadataDB.QueryRowContext(ctx, `SELECT COALESCE(album_name,''), COALESCE(duration,0),
COALESCE(genre,''), year, COALESCE(metadata_source,''), metadata_checked
FROM tracks WHERE id = ?`, pendingID).Scan(&album, &duration, &genre, &year, &source, &checked); err != nil {
		t.Fatalf("read merged track: %v", err)
	}
	if album != "" || duration != 0 {
		t.Errorf("identity should remain unchanged to avoid a duplicate: album=%q duration=%v", album, duration)
	}
	if genre != "Rock" || year != 2020 || source != "deezer" || !checked {
		t.Errorf("non-identity metadata was not merged: genre=%q year=%d source=%q checked=%t", genre, year, source, checked)
	}
	count, err := CountTracks(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count tracks: %v", err)
	}
	if count != 2 {
		t.Fatalf("merge changed the number of tracks, got %d, want 2", count)
	}
}

func TestMergeTrackMetadataKeepsExistingValues(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	id, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Creep", ArtistName: "Radiohead", AlbumName: "Pablo Honey",
		Duration: 238, Genre: "Alternative", Year: 1992, ISRC: "GBAYE9200236",
		MetadataSource: "itunes", Source: "itunes",
		MetadataChecked: true, CoverURLChecked: true,
	})
	if err != nil {
		t.Fatalf("upsert track: %v", err)
	}

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// A sparse provider result must not blank out anything already stored.
	if err := MergeTrackMetadata(ctx, tx, id, Track{
		Name: "Creep", ArtistName: "Radiohead", MetadataSource: "deezer",
		MetadataChecked: true,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("merge metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	track, err := FindTrackMetadataExact(ctx, metadataDB, "creep", "radiohead", "pablo honey", 238)
	if err != nil {
		t.Fatalf("find track: %v", err)
	}
	if track.Genre != "Alternative" {
		t.Errorf("genre was clobbered: %q", track.Genre)
	}
	if track.Year != 1992 {
		t.Errorf("year was clobbered: %d", track.Year)
	}
	if track.ISRC != "GBAYE9200236" {
		t.Errorf("isrc was clobbered: %q", track.ISRC)
	}
}

func TestMergeTrackMetadataDoesNotInsertOrDuplicate(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	id, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Zombie", ArtistName: "The Cranberries", AlbumName: "No Need To Argue",
		Duration: 318, Source: "lrclib_fallback",
	})
	if err != nil {
		t.Fatalf("upsert track: %v", err)
	}
	// Count after the insert so the merge is the only thing that could add rows.
	before, err := CountTracks(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count tracks: %v", err)
	}

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MergeTrackMetadata(ctx, tx, id, Track{
		Name: "Zombie", ArtistName: "The Cranberries", AlbumName: "No Need To Argue",
		Duration: 318, Genre: "Alternative", MetadataSource: "itunes", MetadataChecked: true,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("merge metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, err := CountTracks(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count tracks after merge: %v", err)
	}
	if after != before {
		t.Fatalf("merge must not insert rows: before %d, after %d", before, after)
	}
}

func TestMarkTrackMetadataChecked(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	id, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Unknown Song", ArtistName: "Nobody", AlbumName: "Nowhere",
		Duration: 100, Source: "lrclib_fallback",
	})
	if err != nil {
		t.Fatalf("upsert track: %v", err)
	}

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MarkTrackMetadataChecked(ctx, tx, id); err != nil {
		_ = tx.Rollback()
		t.Fatalf("mark checked: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	remaining, err := CountTracksMissingMetadata(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count missing: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected the track to be marked checked, %d still missing", remaining)
	}

	// The track's existing data must be untouched by the flag.
	track, err := FindTrackMetadataExact(ctx, metadataDB, "unknown song", "nobody", "nowhere", 100)
	if err != nil {
		t.Fatalf("find track: %v", err)
	}
	if !track.MetadataChecked {
		t.Error("expected MetadataChecked to be true")
	}
	if track.Source != "lrclib_fallback" {
		t.Errorf("source changed: %q", track.Source)
	}

	// The timestamp is the only way to age a miss: without it a track marked a
	// year ago is indistinguishable from one marked a minute ago.
	var checkedAt sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT metadata_checked_at FROM tracks WHERE id = ?", id).Scan(&checkedAt); err != nil {
		t.Fatalf("read checked_at: %v", err)
	}
	if !checkedAt.Valid || checkedAt.String == "" {
		t.Fatal("expected metadata_checked_at to be stamped when a track is marked checked")
	}
}

func TestMergeTrackMetadataStampsCheckedAt(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	id, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Resolved Song", ArtistName: "Someone", AlbumName: "Somewhere",
		Duration: 200, Source: "lrclib_fallback",
	})
	if err != nil {
		t.Fatalf("upsert track: %v", err)
	}
	// A track that entered through the lyrics path has no timestamp yet.
	var before sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT metadata_checked_at FROM tracks WHERE id = ?", id).Scan(&before); err != nil {
		t.Fatalf("read before: %v", err)
	}
	if before.Valid {
		t.Fatalf("expected no timestamp before resolution, got %q", before.String)
	}

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MergeTrackMetadata(ctx, tx, id, Track{
		Name: "Resolved Song", ArtistName: "Someone",
		Genre: "Pop", MetadataSource: "itunes",
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("merge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var after sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT metadata_checked_at FROM tracks WHERE id = ?", id).Scan(&after); err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !after.Valid || after.String == "" {
		t.Fatal("expected metadata_checked_at to be stamped when a track is resolved")
	}
}

// ageCheckedAt backdates a track's check stamp and returns the value as the
// driver reads it back.
//
// It has to return the round-tripped form rather than the literal written,
// because the driver parses the column and re-renders it in its own layout.
// Comparing a later read against the literal would fail on formatting alone
// rather than on the behaviour under test.
func ageCheckedAt(t *testing.T, database *sql.DB, trackID int64) string {
	t.Helper()
	if _, err := database.ExecContext(context.Background(),
		"UPDATE tracks SET metadata_checked_at = '2001-02-03 04:05:06' WHERE id = ?", trackID); err != nil {
		t.Fatalf("age the stamp: %v", err)
	}
	var aged sql.NullString
	if err := database.QueryRowContext(context.Background(),
		"SELECT metadata_checked_at FROM tracks WHERE id = ?", trackID).Scan(&aged); err != nil {
		t.Fatalf("read aged stamp: %v", err)
	}
	if aged.String == "" {
		t.Fatal("expected the aged stamp to be readable")
	}
	return aged.String
}

// TestReResolutionRefreshesCheckedAt pins the other half of the contract: a
// genuine second upstream answer must move the timestamp, even though the
// metadata_checked flag was already 1 and therefore does not change.
//
// The flag is monotonic, so "did the flag change" and "did a provider answer"
// are different questions. Only the second one identifies a fresh answer, and
// the age column is only useful if it tracks it.
func TestReResolutionRefreshesCheckedAt(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	// Age the first stamp so a refresh is unambiguous even at second
	// resolution, where two CURRENT_TIMESTAMP values can be identical.
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 300,
		MetadataSource: "itunes", MetadataChecked: true,
	}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	aged := ageCheckedAt(t, metadataDB, 1)

	// A second upstream resolution: same track, same already-set flag.
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 300,
		MetadataSource: "deezer", MetadataChecked: true,
	}); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	var refreshed sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT metadata_checked_at FROM tracks WHERE id = 1").Scan(&refreshed); err != nil {
		t.Fatalf("read refreshed: %v", err)
	}
	if refreshed.String == aged {
		t.Error("a second upstream resolution did not refresh metadata_checked_at")
	}
}

// TestReResolutionRefreshesCheckedAtByRecordingID covers the other write path.
// It updates in place when a MusicBrainz recording ID is already on file, so it
// never goes through the upsert's conflict branch and needs its own coverage.
func TestReResolutionRefreshesCheckedAtByRecordingID(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	recording := "recording-id-1"
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 300,
		MetadataSource: "itunes", MetadataChecked: true,
		MusicBrainzRecordingID: recording,
	}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	aged := ageCheckedAt(t, metadataDB, 1)

	// The same recording ID takes the in-place UPDATE branch.
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 300,
		MetadataSource: "deezer", MetadataChecked: true,
		MusicBrainzRecordingID: recording,
	}); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	var refreshed sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT metadata_checked_at FROM tracks WHERE id = 1").Scan(&refreshed); err != nil {
		t.Fatalf("read refreshed: %v", err)
	}
	if refreshed.String == aged {
		t.Error("a second upstream resolution did not refresh metadata_checked_at on the by-recording-ID path")
	}
}

// TestIncompleteUpsertDoesNotRefreshCheckedAt: the guard is "the caller reported
// an answer", not "a provider was configured". An upsert carrying no answer must
// leave the age alone even when it does have a recording ID and so takes the
// in-place UPDATE branch.
func TestIncompleteUpsertDoesNotRefreshCheckedAt(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	recording := "recording-id-2"
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 300,
		MetadataSource: "itunes", MetadataChecked: true,
		MusicBrainzRecordingID: recording,
	}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	aged := ageCheckedAt(t, metadataDB, 1)

	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 301,
		Source:                 "lrclib_fallback",
		MusicBrainzRecordingID: recording,
	}); err != nil {
		t.Fatalf("unrelated upsert: %v", err)
	}
	var after sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT metadata_checked_at FROM tracks WHERE id = 1").Scan(&after); err != nil {
		t.Fatalf("read after: %v", err)
	}
	if after.String != aged {
		t.Errorf("an upsert carrying no upstream answer rewrote the check time: %q, want %q",
			after.String, aged)
	}
}

func TestUpsertDoesNotRestampAnAlreadyCheckedTrack(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	// A resolved track, stamped the moment it was checked.
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 300,
		MetadataSource: "itunes", MetadataChecked: true,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var first sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT metadata_checked_at FROM tracks WHERE id = 1").Scan(&first); err != nil {
		t.Fatalf("read first: %v", err)
	}
	if !first.Valid {
		t.Fatal("expected the initial resolution to be stamped")
	}

	// The live server upserts a track again for unrelated reasons (new lyrics,
	// a corrected duration). That must not rewrite the check time, or the age of
	// the original upstream lookup is lost and a year-old miss looks fresh.
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{
		Name: "Song", ArtistName: "Artist", AlbumName: "Album", Duration: 300,
		Source: "lrclib_fallback", MetadataChecked: false,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	var second sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT metadata_checked_at FROM tracks WHERE id = 1").Scan(&second); err != nil {
		t.Fatalf("read second: %v", err)
	}
	if second.String != first.String {
		t.Errorf("an unrelated upsert rewrote the check time: %q then %q", first.String, second.String)
	}
}

func TestMigrateAddsCheckedAtToExistingDatabase(t *testing.T) {
	// An existing deployment has a tracks table without the column, so the
	// migration has to add it in place rather than only for fresh databases.
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := Open(path, Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	// Build the real schema, then remove the new column to stand in for a
	// database created before it existed. Hand-writing a minimal table instead
	// would not exercise the same migration path, since the search refresh
	// needs the full column set.
	schema, err := schemaFS.ReadFile("metadata_schema.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if _, err := legacy.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	// The age index covers the column, and SQLite refuses to drop a column an
	// index depends on. A genuinely pre-upgrade database predates both, so the
	// index goes first.
	if _, err := legacy.ExecContext(context.Background(), "DROP INDEX IF EXISTS idx_tracks_metadata_age"); err != nil {
		t.Fatalf("drop age index: %v", err)
	}
	if _, err := legacy.ExecContext(context.Background(), "ALTER TABLE tracks DROP COLUMN metadata_checked_at"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	migrated, err := Open(path, Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer migrated.Close()
	if err := MigrateMetadata(context.Background(), migrated); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var count int
	if err := migrated.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pragma_table_info('tracks') WHERE name = 'metadata_checked_at'").Scan(&count); err != nil {
		t.Fatalf("inspect column: %v", err)
	}
	if count != 1 {
		t.Error("expected the migration to add metadata_checked_at to an existing tracks table")
	}
}

// TestMigrationLeavesPreUpgradeAgesUnknown is the counterpart to the column test
// above: a track that was already checked before the column existed must come
// out of the migration with a NULL, not an invented time.
//
// A backfill here would have to pick a time it cannot know, and the only
// available choice, "now", makes the entire existing backlog of misses look
// freshly settled. That is precisely the population an aging reader exists to
// revisit, so the migration must leave it distinguishable and untouched.
func TestMigrationLeavesPreUpgradeAgesUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := Open(path, Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	schema, err := schemaFS.ReadFile("metadata_schema.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if _, err := legacy.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	// A track the pre-upgrade server had already checked, plus one it had not.
	// The schema seeds no rows, so both have to be inserted here or the
	// assertion below would pass over an empty table.
	if _, err := legacy.ExecContext(context.Background(), `INSERT INTO tracks
(name, name_lower, artist_name, artist_name_lower, album_name, album_name_lower, duration, metadata_checked)
VALUES
('Already Checked', 'already checked', 'Artist', 'artist', 'Album', 'album', 200, 1),
('Never Checked', 'never checked', 'Artist', 'artist', 'Album', 'album', 201, 0)`); err != nil {
		t.Fatalf("seed tracks: %v", err)
	}
	if _, err := legacy.ExecContext(context.Background(), "DROP INDEX IF EXISTS idx_tracks_metadata_age"); err != nil {
		t.Fatalf("drop age index: %v", err)
	}
	if _, err := legacy.ExecContext(context.Background(),
		"ALTER TABLE tracks DROP COLUMN metadata_checked_at"); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	migrated, err := Open(path, Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer migrated.Close()
	if err := MigrateMetadata(context.Background(), migrated); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Re-running must not start inventing timestamps either.
	if err := MigrateMetadata(context.Background(), migrated); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}

	// The already-checked track is the one that matters: it is the population a
	// backfill would have stamped, and the one an aging reader needs to find.
	const preChecked = "Already Checked"
	var checkedAt sql.NullString
	if err := migrated.QueryRowContext(context.Background(),
		"SELECT metadata_checked_at FROM tracks WHERE name = ?", preChecked).Scan(&checkedAt); err != nil {
		t.Fatalf("read pre-checked track: %v", err)
	}
	if checkedAt.Valid {
		t.Errorf("a track checked before the upgrade came out of the migration with the "+
			"invented time %q; its age is unknown and has to stay NULL so an aging reader "+
			"can still find it", checkedAt.String)
	}

	// And it must still be recorded as checked, so the distinction is the age
	// alone rather than the migration having cleared the flag.
	var checked int
	if err := migrated.QueryRowContext(context.Background(),
		"SELECT metadata_checked FROM tracks WHERE name = ?", preChecked).Scan(&checked); err != nil {
		t.Fatalf("read flag: %v", err)
	}
	if checked != 1 {
		t.Error("the migration cleared metadata_checked on a pre-upgrade track")
	}
}
