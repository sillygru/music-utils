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

	database := testHTTPDatabase(t)
	server := NewWithConfig(fallbackConfig(upstream.URL+"/api"), database)
	cleanupHTTPServer(t, server)

	first := performRequest(t, server.Handler, "/api/get?track_name=Remote+Song&artist_name=Remote+Artist")
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

	second := performRequest(t, server.Handler, "/api/get?track_name=Remote+Song&artist_name=Remote+Artist")
	if second.Code != http.StatusOK {
		t.Fatalf("expected cached 200, got %d: %s", second.Code, second.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one upstream request, got %d", calls.Load())
	}

	var source string
	if err := database.QueryRowContext(context.Background(), `SELECT source FROM tracks WHERE name_lower = 'remote song'`).Scan(&source); err != nil {
		t.Fatalf("read cached source: %v", err)
	}
	if source != "lrclib_fallback" {
		t.Fatalf("unexpected cached source: %q", source)
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
	server := NewWithConfig(cfg, testHTTPDatabase(t))
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/get?track_name=Missing&artist_name=Artist")
	if response.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Fatalf("fallback disabled behavior: status=%d calls=%d", response.Code, calls.Load())
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
	server := NewWithConfig(cfg, testHTTPDatabase(t))
	cleanupHTTPServer(t, server)

	started := time.Now()
	response := performRequest(t, server.Handler, "/api/get?track_name=Slow&artist_name=Artist")
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected timeout to return 404, got %d", response.Code)
	}
	if time.Since(started) > time.Second {
		t.Fatal("timeout fallback took too long")
	}
}
