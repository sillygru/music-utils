package db

import (
	"context"
	"database/sql"
	"testing"
)

func TestRichLyricsMigrationAndUpsert(t *testing.T) {
	_, lyricsDB := testDatabases(t)
	ctx := context.Background()

	var tableCount int
	if err := lyricsDB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='lyrics_sync_variants'").Scan(&tableCount); err != nil {
		t.Fatalf("inspect rich lyrics table: %v", err)
	}
	if tableCount != 1 {
		t.Fatalf("expected rich lyrics table, got %d", tableCount)
	}

	rich := RichLyrics{TrackID: 42, Content: "<tt>word</tt>", Format: "TTML", SyncType: "word", Source: "Unison"}
	if err := UpsertRichLyrics(ctx, lyricsDB, rich); err != nil {
		t.Fatalf("upsert rich lyrics: %v", err)
	}
	got, err := FindRichLyrics(ctx, lyricsDB, 42, "word")
	if err != nil {
		t.Fatalf("find rich lyrics: %v", err)
	}
	if got.Content != rich.Content || got.Format != "ttml" || got.SyncType != "word" || got.Source != "unison" || got.Hash == "" {
		t.Fatalf("unexpected rich lyrics: %+v", got)
	}

	// Re-upserting the same provider variant updates it rather than creating a
	// second row, while a different synchronization level remains available.
	rich.Content = "<tt>updated</tt>"
	if err := UpsertRichLyrics(ctx, lyricsDB, rich); err != nil {
		t.Fatalf("update rich lyrics: %v", err)
	}
	if err := UpsertRichLyrics(ctx, lyricsDB, RichLyrics{TrackID: 42, Content: "<tt>syllable</tt>", Format: "ttml", SyncType: "syllable", Source: "unison"}); err != nil {
		t.Fatalf("insert syllable lyrics: %v", err)
	}
	var count int
	if err := lyricsDB.QueryRowContext(ctx, "SELECT count(*) FROM lyrics_sync_variants WHERE track_id=42").Scan(&count); err != nil {
		t.Fatalf("count rich variants: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected two rich variants, got %d", count)
	}
	got, err = FindRichLyrics(ctx, lyricsDB, 42, "")
	if err != nil {
		t.Fatalf("find preferred rich lyrics: %v", err)
	}
	if got.SyncType != "word" || got.Content != "<tt>updated</tt>" {
		t.Fatalf("expected word variant to win, got %+v", got)
	}
	if _, err := FindRichLyrics(ctx, lyricsDB, 999, "word"); err != sql.ErrNoRows {
		t.Fatalf("expected missing rich lyrics, got %v", err)
	}
}
