package db

import (
	"context"
	"database/sql"
	"testing"
)

// A lyrics row in hand is a definitive answer, so the track has to be stamped. If
// it were not, every track the live request path ever served would still read as
// never-settled, and a later refresh could never judge its age.
func TestInsertTrackWithLyricsStampsLyricsChecked(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()

	if _, _, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB,
		Track{Name: "Song", NameLower: "song", ArtistName: "Artist", ArtistNameLower: "artist", AlbumName: "Album", AlbumNameLower: "album", Duration: 200},
		Lyrics{PlainLyrics: "words"}); err != nil {
		t.Fatalf("InsertTrackWithLyrics: %v", err)
	}

	var checked int
	var at sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT lyrics_checked, lyrics_checked_at FROM tracks WHERE name_lower = 'song'").Scan(&checked, &at); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if checked != 1 {
		t.Errorf("lyrics_checked = %d, want 1", checked)
	}
	if !at.Valid {
		t.Error("a stored lyric must record when the answer was given")
	}
}

// An upsert that carries no upstream result must leave the settle time alone, for
// the same reason metadata_checked_at does: an unrelated write must not make an
// old answer look fresh.
func TestUpsertWithoutLyricsLeavesTheSettleTimeAlone(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()

	track := Track{Name: "Song", NameLower: "song", ArtistName: "Artist", ArtistNameLower: "artist",
		AlbumName: "Album", AlbumNameLower: "album", Duration: 200}
	if _, _, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, Lyrics{PlainLyrics: "words"}); err != nil {
		t.Fatalf("InsertTrackWithLyrics: %v", err)
	}
	var first sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT lyrics_checked_at FROM tracks WHERE id = 1").Scan(&first); err != nil {
		t.Fatalf("read first stamp: %v", err)
	}

	// The same track written again with no lyrics flag: a metadata correction.
	track.ID = 1
	track.LyricsChecked = false
	track.Genre = "Rock"
	if _, err := UpsertTrackMetadata(ctx, metadataDB, track); err != nil {
		t.Fatalf("UpsertTrackMetadata: %v", err)
	}

	var after sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT lyrics_checked_at FROM tracks WHERE id = 1").Scan(&after); err != nil {
		t.Fatalf("read second stamp: %v", err)
	}
	if after.String != first.String {
		t.Errorf("an unrelated upsert moved the settle time from %q to %q", first.String, after.String)
	}
}

// The settle flag only ever moves one way, so a write that does not know about
// lyrics cannot clear a track's answer.
func TestLyricsCheckedNeverGoesBackwards(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()

	if _, _, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB,
		Track{Name: "Song", NameLower: "song", ArtistName: "Artist", ArtistNameLower: "artist", AlbumName: "Album", AlbumNameLower: "album", Duration: 200},
		Lyrics{PlainLyrics: "words"}); err != nil {
		t.Fatalf("InsertTrackWithLyrics: %v", err)
	}
	if _, err := UpsertTrackMetadata(ctx, metadataDB,
		Track{ID: 1, Name: "Song", NameLower: "song", ArtistName: "Artist", ArtistNameLower: "artist", AlbumName: "Album", AlbumNameLower: "album", Duration: 200}); err != nil {
		t.Fatalf("UpsertTrackMetadata: %v", err)
	}

	var checked int
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT lyrics_checked FROM tracks WHERE id = 1").Scan(&checked); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if checked != 1 {
		t.Errorf("lyrics_checked = %d, want it to stay 1", checked)
	}
}

// An upgraded database has to gain both columns, and the rows already in it must
// come out unsettled: the pre-upgrade library is the "never settled" work the first
// run exists to look at.
func TestMigrationAddsTheLyricsSettleColumns(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	ctx := context.Background()

	for _, name := range []string{"lyrics_checked", "lyrics_checked_at"} {
		var count int
		if err := metadataDB.QueryRowContext(ctx,
			"SELECT count(*) FROM pragma_table_info('tracks') WHERE name = ?", name).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("tracks.%s is missing after migration", name)
		}
	}
	id := seedMetadataTrack(t, metadataDB, "legacy")
	var checked int
	var at sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT lyrics_checked, lyrics_checked_at FROM tracks WHERE id = ?", id).Scan(&checked, &at); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if checked != 0 {
		t.Errorf("lyrics_checked = %d, want 0 for a pre-upgrade row", checked)
	}
	if at.Valid {
		t.Errorf("lyrics_checked_at = %q, want NULL for a pre-upgrade row", at.String)
	}
}
