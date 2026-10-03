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

// A track can hold one lyrics row per provider, because a batch run asks all of
// them. Only the winner is served, so this pins both halves of that: every answer
// is on disk, and the pointer still addresses the one the job chose.
func TestStoreLyricsVariantsKeepsEveryProviderAndPointsAtTheWinner(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	track := Track{Name: "Midnight City", ArtistName: "M83", AlbumName: "Hurry Up, We're Dreaming", Duration: 243}

	winner := Lyrics{PlainLyrics: "words", SyncedLyrics: "[00:01.00]timed", Source: "synced"}
	variants := []Lyrics{
		{PlainLyrics: "different words", Source: "plain"},
		{PlainLyrics: "words", SyncedLyrics: "[00:01.00]timed", Source: "identical"},
	}
	trackID, lyricsID, err := StoreLyricsVariants(ctx, metadataDB, lyricsDB, track, winner, variants)
	if err != nil {
		t.Fatalf("store variants: %v", err)
	}

	// Three rows are linked, but the third is byte-identical to the winner, so
	// content-addressing folds it into the winner's row: two rows, three
	// associations. This is the behaviour that keeps "store everything" from
	// growing the database by the number of providers.
	var rowCount, associationCount int
	if err := lyricsDB.QueryRowContext(ctx, `SELECT count(*) FROM lyrics`).Scan(&rowCount); err != nil {
		t.Fatalf("count lyrics rows: %v", err)
	}
	if err := lyricsDB.QueryRowContext(ctx, `SELECT count(*) FROM lyrics_tracks WHERE track_id = ?`, trackID).Scan(&associationCount); err != nil {
		t.Fatalf("count associations: %v", err)
	}
	if rowCount != 2 || associationCount != 2 {
		t.Fatalf("got %d lyrics rows and %d associations, want 2 and 2 (the duplicate folds into the winner)", rowCount, associationCount)
	}

	// The pointer addresses the winner, and only the winner.
	storedTrack, storedLyrics, err := FindTrackExact(ctx, metadataDB, lyricsDB, track.Name, track.ArtistName, track.AlbumName, track.Duration)
	if err != nil {
		t.Fatalf("find track: %v", err)
	}
	if storedTrack == nil || storedTrack.LastLyricsID != lyricsID {
		t.Fatalf("last_lyrics_id = %d, want the winner %d", storedTrack.LastLyricsID, lyricsID)
	}
	if storedLyrics == nil || storedLyrics.SyncedLyrics != winner.SyncedLyrics {
		t.Fatalf("served lyrics = %+v, want the winner's synced text", storedLyrics)
	}
	if storedLyrics.Source != "synced" {
		t.Errorf("served source = %q, want the winning provider's name", storedLyrics.Source)
	}

	// The alternative that lost is still on disk, still findable, and still says
	// which provider it came from.
	var variantID int64
	if err := lyricsDB.QueryRowContext(ctx, `SELECT id FROM lyrics WHERE track_id = ? AND source = ?`, trackID, "plain").Scan(&variantID); err != nil {
		t.Fatalf("find the losing variant: %v", err)
	}
	loser, err := FindLyricsByID(ctx, lyricsDB, variantID)
	if err != nil {
		t.Fatalf("read the losing variant: %v", err)
	}
	if loser.PlainLyrics != "different words" || loser.Source != "plain" {
		t.Errorf("losing variant = %+v, want its own text and provider name intact", loser)
	}
}

// A track with several provider answers is still one song, or the stats endpoints
// and TotalCached inflate by the number of providers asked.
func TestCountLyricsTracksCountsTracksNotVariants(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()

	trackID, _, err := StoreLyricsVariants(ctx, metadataDB, lyricsDB,
		Track{Name: "Midnight City", ArtistName: "M83", AlbumName: "Hurry Up, We're Dreaming", Duration: 243},
		Lyrics{PlainLyrics: "words", SyncedLyrics: "[00:01.00]timed", Source: "synced"},
		[]Lyrics{
			{PlainLyrics: "different words", Source: "plain"},
			{Instrumental: false, PlainLyrics: "more words", Source: "second"},
		})
	if err != nil {
		t.Fatalf("store variants: %v", err)
	}

	var associationCount int
	if err := lyricsDB.QueryRowContext(ctx, `SELECT count(*) FROM lyrics_tracks WHERE track_id = ?`, trackID).Scan(&associationCount); err != nil {
		t.Fatalf("count associations: %v", err)
	}
	if associationCount != 3 {
		t.Fatalf("expected 3 stored associations to exercise the collapse, got %d", associationCount)
	}

	count, err := CountLyricsTracks(ctx, lyricsDB)
	if err != nil {
		t.Fatalf("count lyrics tracks: %v", err)
	}
	if count != 1 {
		t.Errorf("CountLyricsTracks = %d, want 1 song", count)
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

func TestGetCacheStats(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	coverDB, err := Open(":memory:", Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover test database: %v", err)
	}
	t.Cleanup(func() { _ = coverDB.Close() })
	ctx := context.Background()
	if err := MigrateCover(ctx, coverDB); err != nil {
		t.Fatalf("migrate cover test database: %v", err)
	}

	stats, err := GetCacheStats(ctx, metadataDB, lyricsDB, coverDB)
	if err != nil {
		t.Fatalf("get cache stats on empty: %v", err)
	}
	if stats.MetadataSongs != 0 || stats.LyricsSongs != 0 || stats.SongCovers != 0 || stats.AlbumCovers != 0 || stats.ArtistCovers != 0 || stats.TotalCovers != 0 || stats.UniqueSongs != 0 || stats.TotalCached != 0 {
		t.Fatalf("expected all zeros, got %+v", stats)
	}

	tracks := []Track{
		{Name: "Song One", ArtistName: "Artist A", AlbumName: "Album A", Duration: 200, CoverURL: "http://cover/1"},
		{Name: "song one", ArtistName: "Artist B", AlbumName: "Album B", Duration: 210, CoverURL: "http://cover/2"},
		{Name: "Song Two", ArtistName: "Artist A", AlbumName: "Album A", Duration: 180},
	}
	for i, track := range tracks {
		if i < 2 {
			_, _, err = InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, Lyrics{PlainLyrics: "lyrics text"})
		} else {
			_, err = UpsertTrackMetadata(ctx, metadataDB, track)
		}
		if err != nil {
			t.Fatalf("insert track %d: %v", i, err)
		}
	}
	if err := UpsertCoverArt(ctx, coverDB, CoverAlbum, "Artist A", "Album A", "http://cover/album", "deezer"); err != nil {
		t.Fatalf("insert album cover: %v", err)
	}
	if err := UpsertCoverArt(ctx, coverDB, CoverArtist, "Artist A", "", "http://cover/artist", "deezer"); err != nil {
		t.Fatalf("insert artist cover: %v", err)
	}

	stats, err = GetCacheStats(ctx, metadataDB, lyricsDB, coverDB)
	if err != nil {
		t.Fatalf("get cache stats: %v", err)
	}
	if stats.MetadataSongs != 3 {
		t.Errorf("expected MetadataSongs=3, got %d", stats.MetadataSongs)
	}
	if stats.UniqueSongs != 2 {
		t.Errorf("expected UniqueSongs=2, got %d", stats.UniqueSongs)
	}
	if stats.LyricsSongs != 2 {
		t.Errorf("expected LyricsSongs=2, got %d", stats.LyricsSongs)
	}
	if stats.SongCovers != 2 {
		t.Errorf("expected SongCovers=2, got %d", stats.SongCovers)
	}
	if stats.AlbumCovers != 1 {
		t.Errorf("expected AlbumCovers=1, got %d", stats.AlbumCovers)
	}
	if stats.ArtistCovers != 1 {
		t.Errorf("expected ArtistCovers=1, got %d", stats.ArtistCovers)
	}
	if stats.TotalCovers != 4 {
		t.Errorf("expected TotalCovers=4, got %d", stats.TotalCovers)
	}
	// TotalCached = MetadataSongs (3) + LyricsSongs (2) + TotalCovers (4) = 9
	if stats.TotalCached != 9 {
		t.Errorf("expected TotalCached=9, got %d", stats.TotalCached)
	}
}
