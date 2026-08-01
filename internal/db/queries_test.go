package db

import (
	"context"
	"database/sql"
	"testing"
)

func testDatabases(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	open := func(label string) *sql.DB {
		database, err := Open(":memory:", Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
		if err != nil {
			t.Fatalf("open %s test database: %v", label, err)
		}
		return database
	}
	metadataDB, lyricsDB := open("metadata"), open("lyrics")
	t.Cleanup(func() { _ = metadataDB.Close(); _ = lyricsDB.Close() })
	ctx := context.Background()
	if err := MigrateMetadata(ctx, metadataDB); err != nil {
		t.Fatalf("migrate metadata test database: %v", err)
	}
	if err := MigrateLyrics(ctx, lyricsDB); err != nil {
		t.Fatalf("migrate lyrics test database: %v", err)
	}
	if err := MigrateMetadata(ctx, metadataDB); err != nil {
		t.Fatalf("run idempotent metadata migration: %v", err)
	}
	if err := MigrateLyrics(ctx, lyricsDB); err != nil {
		t.Fatalf("run idempotent lyrics migration: %v", err)
	}
	return metadataDB, lyricsDB
}

func TestInsertAndFindTrackExact(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()

	trackID, lyricsID, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, Track{
		Name:       "Example Song",
		ArtistName: "Example Artist",
		AlbumName:  "Example Album",
		Duration:   203.5,
	}, Lyrics{
		PlainLyrics:  "These are the words",
		SyncedLyrics: "[00:01.00]These are the words",
		Instrumental: false,
	})
	if err != nil {
		t.Fatalf("insert track with lyrics: %v", err)
	}
	if trackID == 0 || lyricsID == 0 {
		t.Fatalf("expected inserted IDs, got track=%d lyrics=%d", trackID, lyricsID)
	}

	track, lyrics, err := FindTrackExact(ctx, metadataDB, lyricsDB, " example song ", "EXAMPLE ARTIST", "example album", 203.5)
	if err != nil {
		t.Fatalf("find track: %v", err)
	}
	if track.ID != trackID || lyrics.ID != lyricsID {
		t.Fatalf("unexpected IDs: track=%d/%d lyrics=%d/%d", track.ID, trackID, lyrics.ID, lyricsID)
	}
	if track.Name != "Example Song" || lyrics.PlainLyrics != "These are the words" {
		t.Fatalf("unexpected result: %+v / %+v", track, lyrics)
	}
	if !lyrics.HasPlain || !lyrics.HasSynced {
		t.Fatalf("expected lyrics flags to be set: %+v", lyrics)
	}
}

func TestInsertDeduplicatesLyricsContent(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	lyrics := Lyrics{PlainLyrics: "same lyrics"}

	_, firstLyricsID, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, Track{
		Name: "First Song", ArtistName: "Artist", Duration: 100,
	}, lyrics)
	if err != nil {
		t.Fatalf("insert first track: %v", err)
	}
	_, secondLyricsID, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, Track{
		Name: "Second Song", ArtistName: "Artist", Duration: 101,
	}, lyrics)
	if err != nil {
		t.Fatalf("insert second track: %v", err)
	}
	if firstLyricsID != secondLyricsID {
		t.Fatalf("expected content deduplication, got lyrics IDs %d and %d", firstLyricsID, secondLyricsID)
	}

	var associationCount int
	if err := lyricsDB.QueryRowContext(ctx, `SELECT count(*) FROM lyrics_tracks WHERE lyrics_id = ?`, firstLyricsID).Scan(&associationCount); err != nil {
		t.Fatalf("count shared lyrics associations: %v", err)
	}
	if associationCount != 2 {
		t.Fatalf("expected shared lyrics to remain associated with both tracks, got %d associations", associationCount)
	}
}

func TestSearchTracksUsesFTS(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	for _, track := range []Track{
		{Name: "Midnight City", ArtistName: "M83", Duration: 243},
		{Name: "Midnight Train", ArtistName: "Other Artist", Duration: 200},
		{Name: "Daylight", ArtistName: "M83", Duration: 220},
	} {
		if _, _, err := InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, Lyrics{
			PlainLyrics: track.Name + " lyrics",
		}); err != nil {
			t.Fatalf("insert %q: %v", track.Name, err)
		}
	}

	tracks, err := SearchTracks(ctx, metadataDB, lyricsDB, "midnight cit", 20)
	if err != nil {
		t.Fatalf("search tracks: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Name != "Midnight City" {
		t.Fatalf("unexpected search results: %+v", tracks)
	}
}
