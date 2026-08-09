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

func TestCountHelpers(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()

	assertCount := func(label string, got int64, err error, want int64) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got != want {
			t.Fatalf("%s: expected %d, got %d", label, want, got)
		}
	}

	// Empty caches count zero, and album/artist covers are zero without a
	// cover database.
	metadataCount, err := CountTracks(ctx, metadataDB)
	assertCount("count tracks on empty", metadataCount, err, 0)
	nameCount, err := CountDistinctTrackNames(ctx, metadataDB)
	assertCount("count names on empty", nameCount, err, 0)
	lyricsCount, err := CountLyricsTracks(ctx, lyricsDB)
	assertCount("count lyrics on empty", lyricsCount, err, 0)
	coverCounts, err := CountCovers(ctx, metadataDB, nil)
	assertCount("count song covers on empty", coverCounts.Songs, err, 0)
	if coverCounts.Total() != 0 || coverCounts.Albums != 0 || coverCounts.Artists != 0 {
		t.Fatalf("expected zero cover counts, got %+v", coverCounts)
	}

	// Two tracks share one (case-normalized) name; the first two also carry
	// lyrics and a song cover URL, the third is metadata-only. The shared
	// lyrics content deduplicates into one lyrics row across two associations.
	tracks := []Track{
		{Name: "Paranoid Android", ArtistName: "Radiohead", AlbumName: "OK Computer", Duration: 383, CoverURL: "http://cover/1"},
		{Name: "paranoid android", ArtistName: "A Cover Band", AlbumName: "Tribute", Duration: 300, CoverURL: "http://cover/2"},
		{Name: "No Surprises", ArtistName: "Radiohead", AlbumName: "OK Computer", Duration: 229},
	}
	for i, track := range tracks {
		var err error
		if i < 2 {
			_, _, err = InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, Lyrics{PlainLyrics: "shared words"})
		} else {
			_, err = UpsertTrackMetadata(ctx, metadataDB, track)
		}
		if err != nil {
			t.Fatalf("seed track %d: %v", i, err)
		}
	}

	metadataCount, err = CountTracks(ctx, metadataDB)
	assertCount("count tracks", metadataCount, err, 3)
	nameCount, err = CountDistinctTrackNames(ctx, metadataDB)
	assertCount("count distinct names", nameCount, err, 2)
	lyricsCount, err = CountLyricsTracks(ctx, lyricsDB)
	assertCount("count lyrics associations", lyricsCount, err, 2)
	songCovers, err := CountCovers(ctx, metadataDB, nil)
	assertCount("count song covers", songCovers.Songs, err, 2)
	if songCovers.Total() != 2 {
		t.Fatalf("expected song-only cover total 2, got %+v", songCovers)
	}
}

func TestCountCoversIncludesAlbumAndArtist(t *testing.T) {
	metadataDB, _ := testDatabases(t)
	coverDB, err := Open(":memory:", Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover test database: %v", err)
	}
	t.Cleanup(func() { _ = coverDB.Close() })
	ctx := context.Background()
	if err := MigrateCover(ctx, coverDB); err != nil {
		t.Fatalf("migrate cover test database: %v", err)
	}

	// A bare track carrying a song cover URL.
	if _, err := UpsertTrackMetadata(ctx, metadataDB, Track{Name: "Hits", ArtistName: "Artist", Duration: 200, CoverURL: "http://cover/song"}); err != nil {
		t.Fatalf("insert track: %v", err)
	}
	// One positive album cover and one positive artist cover, plus a negative
	// album cover (checked miss) that must not count.
	if err := UpsertCoverArt(ctx, coverDB, CoverAlbum, "Artist", "Album One", "http://cover/album", "deezer"); err != nil {
		t.Fatalf("insert album cover: %v", err)
	}
	if err := UpsertCoverArt(ctx, coverDB, CoverArtist, "Artist", "", "http://cover/artist", "deezer"); err != nil {
		t.Fatalf("insert artist cover: %v", err)
	}
	if err := UpsertCoverArt(ctx, coverDB, CoverAlbum, "Artist", "Missing Album", "", ""); err != nil {
		t.Fatalf("insert negative album cover: %v", err)
	}

	counts, err := CountCovers(ctx, metadataDB, coverDB)
	if err != nil {
		t.Fatalf("count covers: %v", err)
	}
	if counts.Songs != 1 || counts.Albums != 1 || counts.Artists != 1 || counts.Total() != 3 {
		t.Fatalf("unexpected cover counts: %+v", counts)
	}
}

func TestCountHelpersRequireDatabase(t *testing.T) {
	ctx := context.Background()
	if _, err := CountTracks(ctx, nil); err == nil {
		t.Fatal("expected CountTracks(nil) to fail")
	}
	if _, err := CountDistinctTrackNames(ctx, nil); err == nil {
		t.Fatal("expected CountDistinctTrackNames(nil) to fail")
	}
	if _, err := CountLyricsTracks(ctx, nil); err == nil {
		t.Fatal("expected CountLyricsTracks(nil) to fail")
	}
	if _, err := CountCovers(ctx, nil, nil); err == nil {
		t.Fatal("expected CountCovers(nil, nil) to fail")
	}
}
