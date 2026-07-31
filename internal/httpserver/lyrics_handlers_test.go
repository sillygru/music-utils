package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

func testHTTPDatabase(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(":memory:", db.Config{
		MmapSize:     512 * 1024 * 1024,
		CacheSizeKB:  -64000,
		MaxOpenConns: 1,
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return database
}

func seedHTTPTrack(t *testing.T, database *sql.DB) {
	t.Helper()
	_, _, err := db.InsertTrackWithLyrics(context.Background(), database, db.Track{
		Name:       "Example Song",
		ArtistName: "Example Artist",
		AlbumName:  "Example Album",
		Duration:   203.5,
	}, db.Lyrics{
		PlainLyrics:  "These are the words",
		SyncedLyrics: "[00:01.00]These are the words",
		Instrumental: false,
	})
	if err != nil {
		t.Fatalf("seed track: %v", err)
	}
}

func cleanupHTTPServer(t *testing.T, server *http.Server) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
}

func performRequest(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestGetLyrics(t *testing.T) {
	database := testHTTPDatabase(t)
	seedHTTPTrack(t, database)
	server := New("8080", database)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/get?track_name=+EXAMPLE+SONG+&artist_name=example+artist")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}

	var got lyricsResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.TrackName != "Example Song" || got.ArtistName != "Example Artist" || got.AlbumName != "Example Album" {
		t.Fatalf("unexpected metadata: %+v", got)
	}
	if got.Duration != 203.5 || got.PlainLyrics != "These are the words" || got.SyncedLyrics == "" {
		t.Fatalf("unexpected lyrics response: %+v", got)
	}
}

func TestGetLyricsValidationAndMiss(t *testing.T) {
	database := testHTTPDatabase(t)
	seedHTTPTrack(t, database)
	server := New("8080", database)
	cleanupHTTPServer(t, server)

	missing := performRequest(t, server.Handler, "/api/get?artist_name=artist")
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("expected missing track_name to return 400, got %d", missing.Code)
	}
	var missingError apiError
	if err := json.NewDecoder(missing.Body).Decode(&missingError); err != nil {
		t.Fatalf("decode validation error: %v", err)
	}
	if missingError.Code != http.StatusBadRequest || missingError.Message != "track_name is required" {
		t.Fatalf("unexpected validation error: %+v", missingError)
	}

	missing = performRequest(t, server.Handler, "/api/get?track_name=track")
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("expected missing artist_name to return 400, got %d", missing.Code)
	}

	invalidDuration := performRequest(t, server.Handler, "/api/get?track_name=track&artist_name=artist&duration=not-a-number")
	if invalidDuration.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid duration to return 400, got %d", invalidDuration.Code)
	}

	withDuration := performRequest(t, server.Handler, "/api/get?track_name=example+song&artist_name=example+artist&duration=203.5")
	if withDuration.Code != http.StatusOK {
		t.Fatalf("expected matching duration to return 200, got %d", withDuration.Code)
	}

	wrongDuration := performRequest(t, server.Handler, "/api/get?track_name=example+song&artist_name=example+artist&duration=204")
	if wrongDuration.Code != http.StatusNotFound {
		t.Fatalf("expected mismatched duration to return 404, got %d", wrongDuration.Code)
	}

	miss := performRequest(t, server.Handler, "/api/get?track_name=unknown&artist_name=artist")
	if miss.Code != http.StatusNotFound {
		t.Fatalf("expected missing track to return 404, got %d", miss.Code)
	}
	var missError apiError
	if err := json.NewDecoder(miss.Body).Decode(&missError); err != nil {
		t.Fatalf("decode not-found error: %v", err)
	}
	if missError.Code != http.StatusNotFound || missError.Message != "Track not found" {
		t.Fatalf("unexpected not-found error: %+v", missError)
	}
}

func TestSearchLyricsReturnsArrayAndHonorsLimit(t *testing.T) {
	database := testHTTPDatabase(t)
	for _, track := range []db.Track{
		{Name: "Midnight City", ArtistName: "M83", Duration: 243},
		{Name: "Midnight Train", ArtistName: "Other Artist", Duration: 200},
	} {
		if _, _, err := db.InsertTrackWithLyrics(context.Background(), database, track, db.Lyrics{
			PlainLyrics: track.Name + " lyrics",
		}); err != nil {
			t.Fatalf("seed %q: %v", track.Name, err)
		}
	}
	server := New("8080", database)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/search?q=midnight&limit=1")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var got []lyricsResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode search response: %v", err)
	}
	if len(got) != 1 || got[0].TrackName != "Midnight City" {
		t.Fatalf("unexpected search results: %+v", got)
	}

	empty := performRequest(t, server.Handler, "/api/search?q=does-not-exist")
	if empty.Code != http.StatusOK {
		t.Fatalf("expected empty search to return 200, got %d", empty.Code)
	}
	var emptyResults []lyricsResponse
	if err := json.NewDecoder(empty.Body).Decode(&emptyResults); err != nil {
		t.Fatalf("decode empty search response: %v", err)
	}
	if emptyResults == nil || len(emptyResults) != 0 {
		t.Fatalf("expected a JSON empty array, got %+v", emptyResults)
	}

	byFields := performRequest(t, server.Handler, "/api/search?track_name=midnight&artist_name=m83")
	if byFields.Code != http.StatusOK {
		t.Fatalf("expected field search to return 200, got %d", byFields.Code)
	}
}
