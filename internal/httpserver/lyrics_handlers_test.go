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
	"github.com/sillygru/music-utils/internal/lrclib"
)

func testHTTPDatabases(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	metadataDB, err := db.Open(":memory:", db.Config{
		MmapSize:     512 * 1024 * 1024,
		CacheSizeKB:  -64000,
		MaxOpenConns: 1,
	})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = metadataDB.Close() })
	if err := db.MigrateMetadata(context.Background(), metadataDB); err != nil {
		t.Fatalf("migrate metadata test database: %v", err)
	}
	lyricsDB, err := db.Open(":memory:", db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open lyrics test database: %v", err)
	}
	t.Cleanup(func() { _ = lyricsDB.Close() })
	if err := db.MigrateLyrics(context.Background(), lyricsDB); err != nil {
		t.Fatalf("migrate lyrics test database: %v", err)
	}
	return metadataDB, lyricsDB
}

func seedHTTPTrack(t *testing.T, metadataDB, lyricsDB *sql.DB) {
	t.Helper()
	_, _, err := db.InsertTrackWithLyrics(context.Background(), metadataDB, lyricsDB, db.Track{
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
	request.Header.Set("User-Agent", "test-agent/1.0")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestGetLyrics(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	server := New("8080", metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Example+Artist+-+Example+Song+(Official+Music+Video).mp3")
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
	// LRCLIB compatibility: name mirrors the track name and lyricsfile is
	// generated from the cached row without any extra storage.
	if got.Name != got.TrackName {
		t.Fatalf("expected name to mirror trackName, got %q", got.Name)
	}
	wantLyricsFile := "version: '1.0'\n" +
		"metadata:\n" +
		"  title: Example Song\n" +
		"  artist: Example Artist\n" +
		"  album: Example Album\n" +
		"  duration_ms: 203500\n" +
		"  instrumental: false\n" +
		"lines:\n" +
		"- text: These are the words\n" +
		"  start_ms: 1000\n" +
		"plain: |-\n" +
		"  These are the words\n"
	if got.LyricsFile != wantLyricsFile {
		t.Fatalf("unexpected lyricsfile:\ngot:\n%q\nwant:\n%q", got.LyricsFile, wantLyricsFile)
	}
}

func TestGetLyricsValidationAndMiss(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	server := New("8080", metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	missing := performRequest(t, server.Handler, "/api/lyrics/get?artist_name=artist")
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

	missing = performRequest(t, server.Handler, "/api/lyrics/get?track_name=track")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("expected artist-less unknown track to be a miss (404), got %d", missing.Code)
	}

	invalidDuration := performRequest(t, server.Handler, "/api/lyrics/get?track_name=track&artist_name=artist&duration=not-a-number")
	if invalidDuration.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid duration to return 400, got %d", invalidDuration.Code)
	}

	withDuration := performRequest(t, server.Handler, "/api/lyrics/get?track_name=example+song&artist_name=example+artist&duration=203.5")
	if withDuration.Code != http.StatusOK {
		t.Fatalf("expected matching duration to return 200, got %d", withDuration.Code)
	}

	wrongDuration := performRequest(t, server.Handler, "/api/lyrics/get?track_name=example+song&artist_name=example+artist&duration=204")
	if wrongDuration.Code != http.StatusNotFound {
		t.Fatalf("expected mismatched duration to return 404, got %d", wrongDuration.Code)
	}

	miss := performRequest(t, server.Handler, "/api/lyrics/get?track_name=unknown&artist_name=artist")
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
	metadataDB, lyricsDB := testHTTPDatabases(t)
	for _, track := range []db.Track{
		{Name: "Midnight City", ArtistName: "M83", Duration: 243},
		{Name: "Midnight Train", ArtistName: "Other Artist", Duration: 200},
	} {
		if _, _, err := db.InsertTrackWithLyrics(context.Background(), metadataDB, lyricsDB, track, db.Lyrics{
			PlainLyrics: track.Name + " lyrics",
		}); err != nil {
			t.Fatalf("seed %q: %v", track.Name, err)
		}
	}
	server := New("8080", metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/search?q=midnight&limit=1")
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

	empty := performRequest(t, server.Handler, "/api/lyrics/search?q=does-not-exist")
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

	byFields := performRequest(t, server.Handler, "/api/lyrics/search?track_name=midnight&artist_name=m83")
	if byFields.Code != http.StatusOK {
		t.Fatalf("expected field search to return 200, got %d", byFields.Code)
	}
}

func TestGetLyricsArtistOptional(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	server := New("8080", metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=EXAMPLE+SONG")
	if response.Code != http.StatusOK {
		t.Fatalf("expected artist-less lookup to return 200, got %d: %s", response.Code, response.Body.String())
	}
	var got lyricsResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.TrackName != "Example Song" || got.ArtistName != "Example Artist" || got.PlainLyrics != "These are the words" {
		t.Fatalf("unexpected artist-less result: %+v", got)
	}
}

func TestSynthesizedLyricsResultFiltered(t *testing.T) {
	synthesized := lrclib.RemoteResult{TrackName: "radiohead  creep", ArtistName: "radiohead  creep", AlbumName: "radiohead  creep", PlainLyrics: "words"}
	if !synthesizedLyricsResult(synthesized) {
		t.Fatal("expected synthesized row to be flagged")
	}
	real := lrclib.RemoteResult{TrackName: "Radiohead - Creep", ArtistName: "Radiohead", AlbumName: "Radiohead - Creep", PlainLyrics: "words"}
	if synthesizedLyricsResult(real) {
		t.Fatal("expected real row not to be flagged")
	}
	nameless := lrclib.RemoteResult{ArtistName: "Radiohead", PlainLyrics: "words"}
	if !synthesizedLyricsResult(nameless) {
		t.Fatal("expected nameless row to be flagged")
	}
}

func TestMatchLyricsByNameSkipsSynthesized(t *testing.T) {
	results := []lrclib.RemoteResult{
		{TrackName: "radiohead creep", ArtistName: "radiohead creep", AlbumName: "radiohead creep", PlainLyrics: "garbage"},
		{TrackName: "Radiohead - Creep", ArtistName: "Radiohead", AlbumName: "Radiohead - Creep", PlainLyrics: "real lyrics"},
	}
	match := matchLyricsByName(results, "creep", "")
	if match == nil || match.ArtistName != "Radiohead" {
		t.Fatalf("expected real match, got %+v", match)
	}
}
