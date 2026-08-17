package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sillygru/music-utils/internal/config"
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
	if fields.RichSync == nil || fields.RichSync.Format != "ttml" || fields.RichSync.SyncType != "word" || fields.RichSync.Source != "unison" {
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
