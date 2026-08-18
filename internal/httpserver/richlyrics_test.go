package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
)

func TestGetLyricsRichSyncIsOptInAndCached(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/lyrics" {
			t.Fatalf("unexpected rich upstream path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("song") != "Example Song" || r.URL.Query().Get("artist") != "Example Artist" {
			t.Fatalf("unexpected rich upstream query: %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"lyrics":"<tt><p begin=\"0s\"><span begin=\"0s\" end=\"1s\">These</span></p></tt>","format":"ttml","syncType":"word"}}`))
	}))
	defer upstream.Close()

	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	cfg := config.Config{
		Port:                  "8080",
		RateLimitPerSec:       100,
		RateLimitPerMin:       1000,
		FallbackPerMin:        100,
		FallbackMaxQueue:      10,
		FallbackQueueWaitMS:   1000,
		RichLyricsEnabled:     true,
		RichLyricsBaseURL:     upstream.URL,
		RichLyricsUserAgent:   "music-utils-test",
		RichLyricsTimeoutMS:   1000,
		LRCLIBFallbackEnabled: false,
	}
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	legacy := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Example+Song&artist_name=Example+Artist")
	if legacy.Code != http.StatusOK {
		t.Fatalf("expected legacy response 200, got %d: %s", legacy.Code, legacy.Body.String())
	}
	var legacyFields map[string]any
	if err := json.NewDecoder(legacy.Body).Decode(&legacyFields); err != nil {
		t.Fatalf("decode legacy response: %v", err)
	}
	if _, ok := legacyFields["richSync"]; ok {
		t.Fatal("legacy request unexpectedly returned richSync")
	}
	if calls.Load() != 0 {
		t.Fatalf("legacy request unexpectedly called rich provider: %d", calls.Load())
	}

	rich := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Example+Song&artist_name=Example+Artist&include_rich_sync=true&sync_type=word")
	if rich.Code != http.StatusOK {
		t.Fatalf("expected rich response 200, got %d: %s", rich.Code, rich.Body.String())
	}
	var fields struct {
		PlainLyrics  *string         `json:"plainLyrics"`
		SyncedLyrics *string         `json:"syncedLyrics"`
		LyricsFile   *string         `json:"lyricsfile"`
		RichSync     *richSyncResult `json:"richSync"`
	}
	if err := json.NewDecoder(rich.Body).Decode(&fields); err != nil {
		t.Fatalf("decode rich response: %v", err)
	}
	if fields.RichSync == nil || fields.RichSync.Format != "json" || fields.RichSync.SyncType != "word" || fields.RichSync.Source != "unison" {
		t.Fatalf("unexpected rich response: %+v", fields.RichSync)
	}
	if fields.PlainLyrics != nil || fields.SyncedLyrics != nil || fields.LyricsFile != nil {
		t.Fatal("rich response unexpectedly included redundant LRCLIB lyrics fields")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one rich provider call, got %d", calls.Load())
	}

	var stored int
	if err := lyricsDB.QueryRowContext(context.Background(), "SELECT count(*) FROM lyrics_sync_variants WHERE track_id=1").Scan(&stored); err != nil {
		t.Fatalf("inspect stored rich variant: %v", err)
	}
	if stored != 1 {
		t.Fatalf("expected one stored rich variant, got %d", stored)
	}

	cached := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Example+Song&artist_name=Example+Artist&include_rich_sync=true&sync_type=word")
	if cached.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("expected cached rich response, status=%d calls=%d", cached.Code, calls.Load())
	}
}

func TestGetLyricsRichSyncCanResolveWithoutLineLyrics(t *testing.T) {
	var richCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/get" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/lyrics" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		richCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"lyrics":"<tt>rich only</tt>","format":"ttml","syncType":"word"}}`))
	}))
	defer upstream.Close()

	metadataDB, lyricsDB := testHTTPDatabases(t)
	cfg := fallbackConfig(upstream.URL + "/api")
	cfg.RichLyricsEnabled = true
	cfg.RichLyricsBaseURL = upstream.URL
	cfg.RichLyricsUserAgent = "music-utils-test"
	cfg.RichLyricsTimeoutMS = 1000
	metadataDB.SetMaxOpenConns(1)
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Rich+Only&artist_name=Artist&include_rich_sync=true")
	if response.Code != http.StatusOK {
		t.Fatalf("expected rich-only response 200, got %d: %s", response.Code, response.Body.String())
	}
	var got struct {
		TrackName  string          `json:"trackName"`
		Plain      *string         `json:"plainLyrics"`
		Synced     *string         `json:"syncedLyrics"`
		LyricsFile *string         `json:"lyricsfile"`
		Rich       *richSyncResult `json:"richSync"`
	}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode rich-only response: %v", err)
	}
	if got.TrackName != "Rich Only" || got.Plain != nil || got.Synced != nil || got.LyricsFile != nil || got.Rich == nil || got.Rich.Content != "<tt>rich only</tt>" {
		t.Fatalf("unexpected rich-only response: %+v", got)
	}
	if richCalls.Load() != 1 {
		t.Fatalf("expected one rich lookup, got %d", richCalls.Load())
	}
}

func TestSearchLyricsRichSyncIsOptInAndCached(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lyrics" {
			t.Fatalf("unexpected rich upstream path: %s", r.URL.Path)
		}
		calls.Add(1)
		if r.URL.Query().Get("song") != "Example Song" || r.URL.Query().Get("artist") != "Example Artist" {
			t.Fatalf("unexpected rich upstream query: %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"lyrics":"<tt><p begin=\"0s\" end=\"1s\"><span begin=\"0s\" end=\"1s\">These</span></p></tt>","format":"ttml","syncType":"word"}}`))
	}))
	defer upstream.Close()

	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	cfg := config.Config{
		Port:                  "8080",
		RateLimitPerSec:       100,
		RateLimitPerMin:       1000,
		FallbackPerMin:        100,
		FallbackMaxQueue:      10,
		FallbackQueueWaitMS:   1000,
		RichLyricsEnabled:     true,
		RichLyricsBaseURL:     upstream.URL,
		RichLyricsUserAgent:   "music-utils-test",
		RichLyricsTimeoutMS:   1000,
		LRCLIBFallbackEnabled: false,
	}
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	legacy := performRequest(t, server.Handler, "/api/lyrics/search?q=example")
	if legacy.Code != http.StatusOK {
		t.Fatalf("expected legacy search 200, got %d: %s", legacy.Code, legacy.Body.String())
	}
	var legacyResults []lyricsResponse
	if err := json.NewDecoder(legacy.Body).Decode(&legacyResults); err != nil {
		t.Fatalf("decode legacy search: %v", err)
	}
	if len(legacyResults) != 1 || legacyResults[0].RichSync != nil || legacyResults[0].PlainLyrics == "" {
		t.Fatalf("unexpected legacy search result: %+v", legacyResults)
	}
	if calls.Load() != 0 {
		t.Fatalf("legacy search unexpectedly called rich provider: %d", calls.Load())
	}

	rich := performRequest(t, server.Handler, "/api/lyrics/search?q=example&include_rich_sync=true&sync_type=word")
	if rich.Code != http.StatusOK {
		t.Fatalf("expected rich search 200, got %d: %s", rich.Code, rich.Body.String())
	}
	var richResults []lyricsResponse
	if err := json.NewDecoder(rich.Body).Decode(&richResults); err != nil {
		t.Fatalf("decode rich search: %v", err)
	}
	if len(richResults) != 1 || richResults[0].RichSync == nil || richResults[0].RichSync.Format != "json" || richResults[0].RichSync.SyncType != "word" {
		t.Fatalf("unexpected rich search result: %+v", richResults)
	}
	if richResults[0].PlainLyrics != "" || richResults[0].SyncedLyrics != "" {
		t.Fatal("rich search result unexpectedly included LRCLIB fields")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one rich provider call, got %d", calls.Load())
	}

	// A different search URL bypasses the HTTP replay cache but should use the
	// rich variant persisted for the local track.
	cached := performRequest(t, server.Handler, "/api/lyrics/search?track_name=example")
	if cached.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("expected cached rich search result, status=%d calls=%d", cached.Code, calls.Load())
	}
	var cachedResults []lyricsResponse
	if err := json.NewDecoder(cached.Body).Decode(&cachedResults); err != nil {
		t.Fatalf("decode cached rich search: %v", err)
	}
	if len(cachedResults) != 1 || cachedResults[0].RichSync != nil {
		t.Fatal("rich payload should remain opt-in on the cached search URL")
	}

	cachedRich := performRequest(t, server.Handler, "/api/lyrics/search?track_name=example&include_rich_sync=1&sync_type=word")
	if cachedRich.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("expected persisted rich search result, status=%d calls=%d", cachedRich.Code, calls.Load())
	}
	var cachedRichResults []lyricsResponse
	if err := json.NewDecoder(cachedRich.Body).Decode(&cachedRichResults); err != nil {
		t.Fatalf("decode persisted rich search: %v", err)
	}
	if len(cachedRichResults) != 1 || cachedRichResults[0].RichSync == nil {
		t.Fatal("expected persisted rich payload on opt-in search")
	}
}

func TestSearchLyricsRichSyncEnrichesUpstreamResults(t *testing.T) {
	var richCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":77,"trackName":"Remote Song","artistName":"Remote Artist","albumName":"Remote Album","duration":200,"instrumental":false,"plainLyrics":"remote lyrics","syncedLyrics":""}]`))
		case "/lyrics":
			richCalls.Add(1)
			if r.URL.Query().Get("song") != "Remote Song" || r.URL.Query().Get("artist") != "Remote Artist" {
				t.Fatalf("unexpected rich upstream query: %v", r.URL.Query())
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"lyrics":"<tt>remote rich</tt>","format":"ttml","syncType":"word"}}`))
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	metadataDB, lyricsDB := testHTTPDatabases(t)
	cfg := fallbackConfig(upstream.URL + "/api")
	cfg.RichLyricsEnabled = true
	cfg.RichLyricsBaseURL = upstream.URL
	cfg.RichLyricsUserAgent = "music-utils-test"
	cfg.RichLyricsTimeoutMS = 1000
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/search?q=remote&include_rich_sync=true")
	if response.Code != http.StatusOK {
		t.Fatalf("expected rich upstream search 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []lyricsResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode rich upstream search: %v", err)
	}
	if len(results) != 1 || results[0].ID != 77 || results[0].RichSync == nil || results[0].RichSync.Content != "<tt>remote rich</tt>" {
		t.Fatalf("unexpected rich upstream result: %+v", results)
	}
	if richCalls.Load() != 1 {
		t.Fatalf("expected one rich lookup, got %d", richCalls.Load())
	}
	if _, _, err := db.FindTrackExact(context.Background(), metadataDB, lyricsDB, "Remote Song", "Remote Artist", "Remote Album", 200); err == nil {
		t.Fatal("upstream search rich enrichment unexpectedly persisted a local track")
	}
}

func TestGetLyricsRichSyncPassesOnlyUserSuppliedParameters(t *testing.T) {
	var requestedQueries []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lyrics" {
			t.Fatalf("unexpected rich upstream path: %s", r.URL.Path)
		}
		requestedQueries = append(requestedQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"lyrics":"<tt>test</tt>","format":"ttml","syncType":"word"}}`))
	}))
	defer upstream.Close()

	metadataDB, lyricsDB := testHTTPDatabases(t)
	// Seed track with non-empty album and duration in DB
	seedHTTPTrack(t, metadataDB, lyricsDB)

	cfg := config.Config{
		Port:                "8080",
		RichLyricsEnabled:   true,
		RichLyricsBaseURL:   upstream.URL,
		RichLyricsUserAgent: "music-utils-test",
		RichLyricsTimeoutMS: 1000,
	}
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	// Case 1: user provides track_name only (artist and album filled from DB, duration never filled)
	resp := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Example+Song&include_rich_sync=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Code)
	}
	if len(requestedQueries) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(requestedQueries))
	}
	if query := requestedQueries[0]; query != "album=Example+Album&artist=Example+Artist&song=Example+Song" {
		t.Fatalf("expected query with artist/album filled from DB and no duration, got: %s", query)
	}

	// Reset rich cache for track 1
	if _, err := lyricsDB.ExecContext(context.Background(), "DELETE FROM lyrics_sync_variants WHERE track_id=1"); err != nil {
		t.Fatalf("clear rich cache: %v", err)
	}

	// Case 2: user provides track_name and artist_name, but overrides album_name and provides duration
	resp2 := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Example+Song&artist_name=Custom+Artist&album_name=Custom+Album&duration=180&include_rich_sync=true")
	if resp2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.Code)
	}
	if len(requestedQueries) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(requestedQueries))
	}
	if query := requestedQueries[1]; query != "album=Custom+Album&artist=Custom+Artist&duration=180&song=Example+Song" {
		t.Fatalf("expected query with user-provided overrides and duration, got: %s", query)
	}
}
