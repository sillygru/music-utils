package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
)

func fallbackConfig(baseURL string) config.Config {
	return config.Config{
		Port:                  "8080",
		RateLimitPerSec:       100,
		RateLimitPerMin:       1000,
		LRCLIBFallbackEnabled: true,
		LRCLIBBaseURL:         baseURL,
		LRCLIBUserAgent:       "music-utils-test",
		LRCLIBTimeoutMS:       1000,
	}
}

func TestGetLyricsFallsBackAndCaches(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/get" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trackName":"Remote Song","artistName":"Remote Artist","albumName":"Remote Album","duration":200,"instrumental":false,"plainLyrics":"remote lyrics","syncedLyrics":""}`))
	}))
	defer upstream.Close()

	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(fallbackConfig(upstream.URL+"/api"), metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	first := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Remote+Song&artist_name=Remote+Artist")
	if first.Code != http.StatusOK {
		t.Fatalf("expected fallback 200, got %d: %s", first.Code, first.Body.String())
	}
	var response lyricsResponse
	if err := json.NewDecoder(first.Body).Decode(&response); err != nil {
		t.Fatalf("decode fallback response: %v", err)
	}
	if response.PlainLyrics != "remote lyrics" {
		t.Fatalf("unexpected fallback response: %+v", response)
	}

	second := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Remote+Song&artist_name=Remote+Artist")
	if second.Code != http.StatusOK {
		t.Fatalf("expected cached 200, got %d: %s", second.Code, second.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one upstream request, got %d", calls.Load())
	}

	var source string
	if err := metadataDB.QueryRowContext(context.Background(), `SELECT source FROM tracks WHERE name_lower = 'remote song'`).Scan(&source); err != nil {
		t.Fatalf("read cached source: %v", err)
	}
	if source != "lrclib_fallback" {
		t.Fatalf("unexpected cached source: %q", source)
	}
}

func TestGetLyricsDoesNotReturnEmptyCachedLyrics(t *testing.T) {
	var getCalls, searchCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/get":
			getCalls.Add(1)
			if r.URL.Query().Get("album_name") == "Wrong Release" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			// Simulate LRCLIB returning a metadata-only exact record for a
			// release variant: this must not be exposed as a successful hit.
			_, _ = w.Write([]byte(`{"id":14306,"trackName":"No Surprises","artistName":"Radiohead","albumName":"OK Computer","duration":229.12,"instrumental":false,"plainLyrics":"","syncedLyrics":""}`))
		case "/api/search":
			searchCalls.Add(1)
			if got := r.URL.Query().Get("q"); got != "No Surprises Radiohead" {
				t.Fatalf("unexpected search query: %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":34649133,"trackName":"No Surprises","artistName":"Radiohead","albumName":"No Surprises","duration":275,"instrumental":false,"plainLyrics":"A heart's full lyrics","syncedLyrics":"[00:25.65]A heart's full lyrics"}]`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	metadataDB, lyricsDB := testHTTPDatabases(t)
	// Seed the same metadata shape as the public instance: it has an empty
	// lyrics row, which must be refreshed from LRCLIB rather than returned.
	if _, _, err := db.InsertTrackWithLyrics(context.Background(), metadataDB, lyricsDB, db.Track{
		Name:       "No Surprises",
		ArtistName: "Radiohead",
		AlbumName:  "OK Computer",
		Duration:   229.12,
	}, db.Lyrics{}); err != nil {
		t.Fatalf("seed empty lyrics row: %v", err)
	}
	server := NewWithConfig(fallbackConfig(upstream.URL+"/api"), metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=No+Surprises&artist_name=Radiohead&album_name=OK+Computer")
	if response.Code != http.StatusOK {
		t.Fatalf("expected refreshed 200, got %d: %s", response.Code, response.Body.String())
	}
	var got lyricsResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode refreshed response: %v", err)
	}
	if got.PlainLyrics != "A heart's full lyrics" || got.AlbumName != "OK Computer" {
		t.Fatalf("expected populated requested track, got %+v", got)
	}
	if getCalls.Load() != 1 || searchCalls.Load() != 1 {
		t.Fatalf("expected one exact and one search request, got exact=%d search=%d", getCalls.Load(), searchCalls.Load())
	}

	second := performRequest(t, server.Handler, "/api/lyrics/get?track_name=No+Surprises&artist_name=Radiohead&album_name=OK+Computer")
	if second.Code != http.StatusOK {
		t.Fatalf("expected refreshed result to be cached, got %d: %s", second.Code, second.Body.String())
	}
	var cached lyricsResponse
	if err := json.NewDecoder(second.Body).Decode(&cached); err != nil {
		t.Fatalf("decode cached response: %v", err)
	}
	if cached.PlainLyrics != "A heart's full lyrics" || cached.AlbumName != "OK Computer" {
		t.Fatalf("expected cached populated result, got %+v", cached)
	}
	if getCalls.Load() != 1 || searchCalls.Load() != 1 {
		t.Fatalf("expected no repeat upstream calls, got exact=%d search=%d", getCalls.Load(), searchCalls.Load())
	}

	// A strict exact miss must use the same search fallback.
	strict := performRequest(t, server.Handler, "/api/lyrics/get?track_name=No+Surprises&artist_name=Radiohead&album_name=Wrong+Release")
	if strict.Code != http.StatusOK {
		t.Fatalf("expected strict exact miss to fall back to 200, got %d: %s", strict.Code, strict.Body.String())
	}
	if getCalls.Load() != 2 || searchCalls.Load() != 2 {
		t.Fatalf("expected one additional exact/search pair, got exact=%d search=%d", getCalls.Load(), searchCalls.Load())
	}
}

func TestGetLyricsFallbackDisabledDoesNotCallUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	cfg := fallbackConfig(upstream.URL + "/api")
	cfg.LRCLIBFallbackEnabled = false
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Missing&artist_name=Artist")
	if response.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Fatalf("fallback disabled behavior: status=%d calls=%d", response.Code, calls.Load())
	}
}

func TestGetLyricsNegativeCacheSkipsUpstreamRepeat(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(fallbackConfig(upstream.URL+"/api"), metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	for i := 0; i < 3; i++ {
		response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Ghost+Song&artist_name=Ghost+Artist")
		if response.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d: %s", response.Code, response.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one upstream request after negative caching, got %d", calls.Load())
	}
}

func TestGetLyricsFallbackTimeoutReturnsNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := fallbackConfig(upstream.URL + "/api")
	cfg.LRCLIBTimeoutMS = 10
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	started := time.Now()
	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Slow&artist_name=Artist")
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected timeout to return 404, got %d", response.Code)
	}
	if time.Since(started) > time.Second {
		t.Fatal("timeout fallback took too long")
	}
}
