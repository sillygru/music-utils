package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"
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
