package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
)

func TestGetMetadataArtistOptional(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	server := New("8080", metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/metadata/get?track_name=Example+Artist+-+Example+Song+(Official+Music+Video).mp3")
	if response.Code != http.StatusOK {
		t.Fatalf("expected artist-less metadata lookup to return 200, got %d: %s", response.Code, response.Body.String())
	}
	var got metadataResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.TrackName != "Example Song" || got.ArtistName != "Example Artist" || got.AlbumName != "Example Album" {
		t.Fatalf("unexpected artist-less metadata result: %+v", got)
	}

	missingTrack := performRequest(t, server.Handler, "/api/metadata/get?artist_name=Example+Artist")
	if missingTrack.Code != http.StatusBadRequest {
		t.Fatalf("expected missing track_name to be 400, got %d", missingTrack.Code)
	}
}

func TestSearchMetadataPrefersLocalExactMatchOverProviderPage(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
		Name:       "Witches Burn",
		ArtistName: "The Pretty Reckless",
		AlbumName:  "Death by Rock and Roll",
		Duration:   294,
	}); err != nil {
		t.Fatalf("seed track: %v", err)
	}

	// A provider page of the same artist that omits the requested title, the
	// shape iTunes returns for a term it cannot match exactly.
	decoys := []*db.Track{
		{Name: "Make Me Wanna Die", ArtistName: "The Pretty Reckless", AlbumName: "Light Me Up"},
		{Name: "Burn", ArtistName: "The Pretty Reckless", AlbumName: "Going to Hell (Deluxe Edition)"},
	}
	resolver := metadata.NewResolver(&metadataStubProvider{name: "itunes", tracks: decoys})
	handler := searchMetadataHandlerWithUpstream(metadataDB, resolver, testFallbackGuard(), true)

	response := performRequest(t, handler, "/api/metadata/search?q=The+Pretty+Reckless+Witches+Burn&limit=12")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []metadataResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected the local match plus both provider hits, got %d: %+v", len(results), results)
	}
	if results[0].TrackName != "Witches Burn" {
		t.Fatalf("result 0 = %q, want the local exact match %q", results[0].TrackName, "Witches Burn")
	}
	// The provider must still be queried and ranked behind the local match.
	if results[1].TrackName != "Make Me Wanna Die" || results[2].TrackName != "Burn" {
		t.Fatalf("provider results missing or reordered: %+v", results)
	}
}
