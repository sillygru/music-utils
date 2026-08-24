package httpserver

import (
	"encoding/json"
	"testing"
)

func TestSearchResponsePreservesDistinctProviderVariants(t *testing.T) {
	response := lyricsResponse{TrackName: "Song", ArtistName: "Artist", PlainLyrics: "plain"}
	line := lyricsResponse{TrackName: "Song", ArtistName: "Artist", PlainLyrics: "plain 2", SyncedLyrics: "[00:01.00]line"}
	rich := lyricsResponse{TrackName: "Song", ArtistName: "Artist", RichSync: &richSyncResult{Source: "apple_music", Format: "json", SyncType: "word", Content: "word"}}

	appendLyricsVariant(&response, &response)
	mergeSearchResponse(&response, &line)
	mergeSearchResponse(&response, &rich)
	mergeSearchResponse(&response, &rich)

	if len(response.Variants) != 3 {
		t.Fatalf("expected three variants, got %d: %+v", len(response.Variants), response.Variants)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded lyricsResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Variants) != 3 {
		t.Fatalf("expected three variants after cache round trip, got %d", len(decoded.Variants))
	}
	if decoded.Variants[0].Provider != "lrclib" || decoded.Variants[1].Provider != "lrclib" || decoded.Variants[2].Provider != "apple_music" {
		t.Fatalf("unexpected variants: %+v", decoded.Variants)
	}
}
