package db

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// trackRow inserts a bare track with no lyrics and returns its id.
func trackRow(t *testing.T, metadataDB *sql.DB, name string) int64 {
	t.Helper()
	id, err := UpsertTrackMetadata(context.Background(), metadataDB, Track{
		Name:       name,
		ArtistName: "Artist",
		AlbumName:  "Album",
	})
	if err != nil {
		t.Fatalf("upsert track %q: %v", name, err)
	}
	return id
}

// An answer has to become visible to the request path as soon as it is written,
// not only once the whole song is finished. The request path resolves a track's
// lyrics only through last_lyrics_id, so a row stored without it is invisible to
// the server no matter how good it is.
func TestStoreLyricsAnswerMakesTheLyricsReachable(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Answer")

	lyricsID, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{
		PlainLyrics: "la la la",
		Source:      "lrclib",
	})
	if err != nil {
		t.Fatalf("store answer: %v", err)
	}
	pointed, err := PointTrackAtBestLyrics(ctx, metadataDB, lyricsDB, trackID, []int64{lyricsID})
	if err != nil {
		t.Fatalf("point track: %v", err)
	}
	if pointed != lyricsID {
		t.Fatalf("pointed at %d, want %d", pointed, lyricsID)
	}

	track, lyrics, err := FindTrackExact(ctx, metadataDB, lyricsDB, "Answer", "Artist", "Album", 0)
	if err != nil {
		t.Fatalf("find track exact: %v", err)
	}
	if track.LastLyricsID != lyricsID {
		t.Errorf("last_lyrics_id = %d, want %d", track.LastLyricsID, lyricsID)
	}
	if lyrics.PlainLyrics != "la la la" {
		t.Errorf("lyrics = %q, want the stored answer", lyrics.PlainLyrics)
	}
	if lyrics.Source != "lrclib" {
		t.Errorf("source = %q, want lrclib: a stored answer must say which provider gave it", lyrics.Source)
	}
}

// Storing an answer is not settling the song. If it were, an interrupted run would
// leave songs marked answered on the strength of one provider out of six.
func TestStoreLyricsAnswerDoesNotSettleTheTrack(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Unsettled")

	if _, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{PlainLyrics: "words", Source: "kugou"}); err != nil {
		t.Fatalf("store answer: %v", err)
	}

	var checked bool
	if err := metadataDB.QueryRowContext(ctx, "SELECT lyrics_checked FROM tracks WHERE id=?", trackID).Scan(&checked); err != nil {
		t.Fatalf("read lyrics_checked: %v", err)
	}
	if checked {
		t.Error("storing one provider's answer settled the track")
	}
}

// The pointer may only move to a strict improvement. A run that resumed and drew
// only plain lyrics must not displace synced lyrics an earlier run had stored.
func TestPointTrackAtBestLyricsNeverDowngrades(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Downgrade")

	synced, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{
		PlainLyrics:  "words",
		SyncedLyrics: "[00:01.00]words",
		Source:       "lrclib",
	})
	if err != nil {
		t.Fatalf("store synced answer: %v", err)
	}
	if _, err := PointTrackAtBestLyrics(ctx, metadataDB, lyricsDB, trackID, []int64{synced}); err != nil {
		t.Fatalf("point at synced: %v", err)
	}

	plain, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{PlainLyrics: "words", Source: "kugou"})
	if err != nil {
		t.Fatalf("store plain answer: %v", err)
	}
	pointed, err := PointTrackAtBestLyrics(ctx, metadataDB, lyricsDB, trackID, []int64{plain})
	if err != nil {
		t.Fatalf("point at plain: %v", err)
	}
	if pointed != 0 {
		t.Errorf("the pointer moved to %d, want it left on the synced answer", pointed)
	}

	var served int64
	if err := metadataDB.QueryRowContext(ctx, "SELECT last_lyrics_id FROM tracks WHERE id=?", trackID).Scan(&served); err != nil {
		t.Fatalf("read last_lyrics_id: %v", err)
	}
	if served != synced {
		t.Errorf("served row = %d, want the synced answer %d", served, synced)
	}
}

// An equal-quality answer must not move the pointer either, or a refresh run would
// flip a track between two equally good answers on every pass.
func TestPointTrackAtBestLyricsLeavesAnEqualAnswerAlone(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Tie")

	first, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{PlainLyrics: "one", Source: "lrclib"})
	if err != nil {
		t.Fatalf("store first answer: %v", err)
	}
	if _, err := PointTrackAtBestLyrics(ctx, metadataDB, lyricsDB, trackID, []int64{first}); err != nil {
		t.Fatalf("point at first: %v", err)
	}

	second, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{PlainLyrics: "two", Source: "kugou"})
	if err != nil {
		t.Fatalf("store second answer: %v", err)
	}
	pointed, err := PointTrackAtBestLyrics(ctx, metadataDB, lyricsDB, trackID, []int64{second})
	if err != nil {
		t.Fatalf("point at second: %v", err)
	}
	if pointed != 0 {
		t.Errorf("the pointer moved to %d on an equal-quality answer, want it left alone", pointed)
	}
}

// A song whose synced answer arrives second still has to end up served, which is
// the case the whole two-step write exists for: the row lands on its own, and the
// pointer is corrected when it is known to be better.
func TestPointTrackAtBestLyricsPromotesALateBetterAnswer(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Promote")

	plain, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{PlainLyrics: "words", Source: "lrclib"})
	if err != nil {
		t.Fatalf("store plain answer: %v", err)
	}
	if _, err := PointTrackAtBestLyrics(ctx, metadataDB, lyricsDB, trackID, []int64{plain}); err != nil {
		t.Fatalf("point at plain: %v", err)
	}

	synced, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{
		PlainLyrics:  "words",
		SyncedLyrics: "[00:01.00]words",
		Source:       "paxsenix",
	})
	if err != nil {
		t.Fatalf("store synced answer: %v", err)
	}
	pointed, err := PointTrackAtBestLyrics(ctx, metadataDB, lyricsDB, trackID, []int64{synced})
	if err != nil {
		t.Fatalf("point at synced: %v", err)
	}
	if pointed != synced {
		t.Errorf("pointed at %d, want the synced answer %d", pointed, synced)
	}
}

// Settling reads the track's whole stored history, not just what one run wrote. A
// run that resumed a song whose better answer came from an earlier pass has to be
// able to find it, or it would point the track at the worse of the two.
func TestSettleTrackLyricsPromotesAnEarlierRunsAnswer(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Earlier")

	synced, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{
		PlainLyrics:  "words",
		SyncedLyrics: "[00:01.00]words",
		Source:       "lyricsplus",
	})
	if err != nil {
		t.Fatalf("store earlier synced answer: %v", err)
	}
	if _, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{PlainLyrics: "words", Source: "kugou"}); err != nil {
		t.Fatalf("store this run's plain answer: %v", err)
	}

	if err := SettleTrackLyrics(ctx, metadataDB, lyricsDB, trackID); err != nil {
		t.Fatalf("settle track: %v", err)
	}

	var served int64
	if err := metadataDB.QueryRowContext(ctx, "SELECT last_lyrics_id FROM tracks WHERE id=?", trackID).Scan(&served); err != nil {
		t.Fatalf("read last_lyrics_id: %v", err)
	}
	if served != synced {
		t.Errorf("served row = %d, want the synced answer stored earlier (%d)", served, synced)
	}
}

// A song no provider has lyrics for is still settled. "Every provider says this
// song has no lyrics" is an answer, and without recording it the run would ask the
// same question of every provider on every pass forever.
func TestSettleTrackLyricsSettlesASongWithNothingStored(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Nothing")

	if err := SettleTrackLyrics(ctx, metadataDB, lyricsDB, trackID); err != nil {
		t.Fatalf("settle track: %v", err)
	}

	var checked bool
	var settledAt sql.NullString
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT lyrics_checked, lyrics_checked_at FROM tracks WHERE id=?", trackID).Scan(&checked, &settledAt); err != nil {
		t.Fatalf("read settle state: %v", err)
	}
	if !checked {
		t.Error("a song every provider missed was not settled")
	}
	if !settledAt.Valid {
		t.Error("lyrics_checked_at was left empty, so a refresh would read this as older than any answer")
	}

	// And it must be gone from the pending set, which is what stops the next run
	// asking again.
	pending, err := CountTracksMissingLyrics(ctx, metadataDB)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("pending = %d after settling, want 0", pending)
	}
}

// The resume read is the whole basis of picking a partway song back up, so it has
// to report successes and misses alike and say which track each belongs to.
func TestListProviderFetchesForTracksSeparatesTracksAndProviders(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	first := trackRow(t, metadataDB, "First")
	second := trackRow(t, metadataDB, "Second")
	untouched := trackRow(t, metadataDB, "Untouched")

	if err := UpsertProviderFetch(ctx, lyricsDB, first, "lrclib", true); err != nil {
		t.Fatalf("record lrclib: %v", err)
	}
	if err := UpsertProviderFetch(ctx, lyricsDB, first, "kugou", false); err != nil {
		t.Fatalf("record kugou: %v", err)
	}
	if err := UpsertProviderFetch(ctx, lyricsDB, second, "lrclib", false); err != nil {
		t.Fatalf("record second lrclib: %v", err)
	}

	fetches, err := ListProviderFetchesForTracks(ctx, lyricsDB, []int64{first, second, untouched, 0, -1})
	if err != nil {
		t.Fatalf("list provider fetches: %v", err)
	}
	if got := len(fetches[first]); got != 2 {
		t.Errorf("first track has %d providers, want 2", got)
	}
	if !fetches[first]["lrclib"] {
		t.Error("first track's lrclib fetch should be recorded as a success")
	}
	if fetches[first]["kugou"] {
		t.Error("first track's kugou fetch should be recorded as a miss")
	}
	if got := len(fetches[second]); got != 1 {
		t.Errorf("second track has %d providers, want 1", got)
	}
	if fetches[second]["lrclib"] {
		t.Error("second track's lrclib fetch should be recorded as a miss")
	}
	if _, ok := fetches[untouched]; ok {
		t.Error("a track with no ledger rows must not appear in the result")
	}
}

// Unlike the request path's read, the resume read applies no freshness window. A
// track still flagged as unsettled that has ledger rows is by definition an
// interrupted run, and its rows describe work that does not need repeating however
// long ago it was written.
func TestListProviderFetchesForTracksIgnoresAge(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Old")

	if err := UpsertProviderFetch(ctx, lyricsDB, trackID, "lrclib", true); err != nil {
		t.Fatalf("record fetch: %v", err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	if _, err := lyricsDB.ExecContext(ctx,
		"UPDATE lyrics_provider_fetches SET last_fetched_at=? WHERE track_id=? AND provider=?",
		old, trackID, "lrclib"); err != nil {
		t.Fatalf("age the ledger row: %v", err)
	}

	fetches, err := ListProviderFetchesForTracks(ctx, lyricsDB, []int64{trackID})
	if err != nil {
		t.Fatalf("list provider fetches: %v", err)
	}
	if !fetches[trackID]["lrclib"] {
		t.Error("an ancient ledger row was dropped, so an interrupted song would be re-asked from scratch")
	}
	// The request path's own window must still apply, or a server would go on
	// skipping a provider it already asked weeks ago.
	recent, err := ListRecentProviderFetches(ctx, lyricsDB, trackID, ProviderFetchStaleTTL)
	if err != nil {
		t.Fatalf("list recent provider fetches: %v", err)
	}
	if _, ok := recent["lrclib"]; ok {
		t.Error("the request path should still consider an ancient row stale")
	}
}

func TestListProviderFetchesForTracksToleratesNoInput(t *testing.T) {
	_, lyricsDB := testDatabases(t)
	ctx := context.Background()

	fetches, err := ListProviderFetchesForTracks(ctx, nil, []int64{1})
	if err != nil {
		t.Fatalf("nil database should be tolerated: %v", err)
	}
	if len(fetches) != 0 {
		t.Errorf("fetches = %v, want empty", fetches)
	}
	fetches, err = ListProviderFetchesForTracks(ctx, lyricsDB, nil)
	if err != nil {
		t.Fatalf("no ids should be tolerated: %v", err)
	}
	if len(fetches) != 0 {
		t.Errorf("fetches = %v, want empty", fetches)
	}
}

// The identical-text dedup is what stops six providers answering the same popular
// song from growing the database six times, and the settle still has to work when
// that happens: the deduped row belongs to whichever provider wrote first, but the
// track is still answered.
func TestSettleTrackLyricsHandlesDeduplicatedAnswers(t *testing.T) {
	metadataDB, lyricsDB := testDatabases(t)
	ctx := context.Background()
	trackID := trackRow(t, metadataDB, "Popular")

	first, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{PlainLyrics: "same words", Source: "lrclib"})
	if err != nil {
		t.Fatalf("store first answer: %v", err)
	}
	second, err := StoreLyricsAnswer(ctx, lyricsDB, trackID, Lyrics{PlainLyrics: "same words", Source: "kugou"})
	if err != nil {
		t.Fatalf("store second answer: %v", err)
	}
	if first != second {
		t.Fatalf("identical text stored as two rows (%d and %d), want one", first, second)
	}

	if err := SettleTrackLyrics(ctx, metadataDB, lyricsDB, trackID); err != nil {
		t.Fatalf("settle track: %v", err)
	}
	var served int64
	if err := metadataDB.QueryRowContext(ctx, "SELECT last_lyrics_id FROM tracks WHERE id=?", trackID).Scan(&served); err != nil {
		t.Fatalf("read last_lyrics_id: %v", err)
	}
	if served != first {
		t.Errorf("served row = %d, want the deduplicated row %d", served, first)
	}
}
