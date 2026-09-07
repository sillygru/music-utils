package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sillygru/music-utils/internal/db"
)

const testProvidersTTML = `<tt xmlns="http://www.w3.org/ns/ttml"><body><div>` +
	`<p begin="00:01.00" end="00:03.00">better line one</p>` +
	`<p begin="00:04.00" end="00:06.00">better line two</p>` +
	`</div></body></tt>`

func TestGetLyricsFansOutToBetterLyrics(t *testing.T) {
	lrclib404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer lrclib404.Close()
	var betterCalls atomic.Int32
	better := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betterCalls.Add(1)
		if r.URL.Path != "/getLyrics" {
			t.Errorf("unexpected betterlyrics path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ttml":` + jsonQuote(testProvidersTTML) + `}`))
	}))
	defer better.Close()

	cfg := fallbackConfig(lrclib404.URL + "/api")
	cfg.BetterLyricsEnabled = true
	cfg.BetterLyricsBaseURL = better.URL
	cfg.BetterLyricsUserAgent = "music-utils-test"
	cfg.BetterLyricsTimeoutMS = 2000
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Better+Song&artist_name=Better+Artist")
	if response.Code != http.StatusOK {
		t.Fatalf("expected betterlyrics 200, got %d: %s", response.Code, response.Body.String())
	}
	var got lyricsResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.SyncedLyrics, "[00:01.00]better line one") {
		t.Fatalf("unexpected synced lyrics: %q", got.SyncedLyrics)
	}
	if betterCalls.Load() < 1 {
		t.Fatalf("expected betterlyrics to be called")
	}
	var source string
	if err := metadataDB.QueryRowContext(context.Background(), `SELECT source FROM tracks WHERE name_lower = 'better song'`).Scan(&source); err != nil {
		t.Fatalf("read cached source: %v", err)
	}
	if source != "betterlyrics" {
		t.Fatalf("unexpected cached source: %q", source)
	}
}

func TestGetLyricsVideoIDGating(t *testing.T) {
	lrclib404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer lrclib404.Close()
	var zemerCalls atomic.Int32
	zemerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zemerCalls.Add(1)
		_, _ = w.Write([]byte(`{"videoId":"vid1234567","sources":[` +
			`{"type":"canonical","plain":"zemer one\nzemer two\nzemer three\nzemer four"}]}`))
	}))
	defer zemerServer.Close()

	cfg := fallbackConfig(lrclib404.URL + "/api")
	cfg.ZemerEnabled = true
	cfg.ZemerBaseURL = zemerServer.URL
	cfg.ZemerUserAgent = "music-utils-test"
	cfg.ZemerTimeoutMS = 2000
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	// Without video_id the video-keyed provider must not be consulted.
	miss := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Video+Song&artist_name=Video+Artist")
	if miss.Code != http.StatusNotFound {
		t.Fatalf("expected miss 404 without video_id, got %d: %s", miss.Code, miss.Body.String())
	}
	if zemerCalls.Load() != 0 {
		t.Fatalf("zemer must not be called without video_id, got %d calls", zemerCalls.Load())
	}

	// With video_id the Zemer result resolves and caches.
	hit := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Video+Song&artist_name=Video+Artist&video_id=vid1234567")
	if hit.Code != http.StatusOK {
		t.Fatalf("expected zemer 200 with video_id, got %d: %s", hit.Code, hit.Body.String())
	}
	var got lyricsResponse
	if err := json.NewDecoder(hit.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.PlainLyrics, "zemer four") {
		t.Fatalf("unexpected plain lyrics: %q", got.PlainLyrics)
	}

	// An invalid video_id is ignored, never forwarded.
	before := zemerCalls.Load()
	bad := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Other+Song&artist_name=Other+Artist&video_id=!!!invalid!!!")
	if bad.Code != http.StatusNotFound {
		t.Fatalf("expected miss 404 with invalid video_id, got %d", bad.Code)
	}
	if zemerCalls.Load() != before {
		t.Fatalf("invalid video_id must not reach zemer")
	}
}

func TestGetLyricsKugouFallback(t *testing.T) {
	lrclib404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer lrclib404.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/search/song", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":1,"errcode":0,"error":"","data":{"info":[{"duration":200,"hash":"h1"}]}}`))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"id":5,"product_from":"x","duration":200000,"accesskey":"k"}]}`))
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":"WzAwOjAxLjAwXWt1Z291IGxpbmUgb25lClswMDowMi4wMF1rdWdvdSBsaW5lIHR3bwpbMDA6MDMuMDBda3Vnb3UgbGluZSB0aHJlZQpbMDA6MDQuMDBda3Vnb3UgbGluZSBmb3VyCg=="}`))
	})
	kugouServer := httptest.NewServer(mux)
	defer kugouServer.Close()

	cfg := fallbackConfig(lrclib404.URL + "/api")
	cfg.KugouEnabled = true
	cfg.KugouSearchBaseURL = kugouServer.URL
	cfg.KugouLyricsBaseURL = kugouServer.URL
	cfg.KugouUserAgent = "music-utils-test"
	cfg.KugouTimeoutMS = 2000
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Kugou+Song&artist_name=Kugou+Artist")
	if response.Code != http.StatusOK {
		t.Fatalf("expected kugou 200, got %d: %s", response.Code, response.Body.String())
	}
	var got lyricsResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.SyncedLyrics, "kugou line") {
		t.Fatalf("unexpected synced lyrics: %q", got.SyncedLyrics)
	}
	var lyrics db.Lyrics
	track, lyricsPtr, err := db.FindTrackExact(context.Background(), metadataDB, lyricsDB, "Kugou Song", "Kugou Artist", "", 0)
	if err != nil {
		t.Fatalf("find cached: %v", err)
	}
	_ = track
	lyrics = *lyricsPtr
	if lyrics.Source != "kugou" {
		t.Fatalf("unexpected cached lyrics source: %q", lyrics.Source)
	}
}

func jsonQuote(s string) string {
	out := strings.ReplaceAll(s, `\`, `\\`)
	return `"` + strings.ReplaceAll(out, `"`, `\"`) + `"`
}
