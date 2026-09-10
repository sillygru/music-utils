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

// TestSearchUpstreamWhenArtistHasOtherCachedSongs verifies that searching for a new song
// by an artist who already has multiple other songs cached in the local database
// still calls upstream and merges/returns the upstream results.
func TestSearchUpstreamWhenArtistHasOtherCachedSongs(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)

	// Seed 5 existing songs by "Coldplay"
	existingSongs := []string{"Yellow", "Fix You", "The Scientist", "Clocks", "Paradise"}
	for _, title := range existingSongs {
		_, _, err := db.InsertTrackWithLyrics(context.Background(), metadataDB, lyricsDB, db.Track{
			Name:       title,
			ArtistName: "Coldplay",
			AlbumName:  "Album",
			Duration:   240,
		}, db.Lyrics{
			PlainLyrics:  "lyrics for " + title,
			SyncedLyrics: "[00:01.00]lyrics for " + title,
		})
		if err != nil {
			t.Fatalf("seed track %s: %v", title, err)
		}
	}

	var upstreamSearches atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" {
			upstreamSearches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":999,"trackName":"Viva La Vida","artistName":"Coldplay","albumName":"Viva La Vida or Death and All His Friends","duration":242,"instrumental":false,"plainLyrics":"I used to rule the world","syncedLyrics":"[00:01.00]I used to rule the world"}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	cfg := fallbackConfig(upstream.URL + "/api")
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	// Search for a song not in local DB, with include_rich_sync=true
	resp := performRequest(t, server.Handler, "/api/lyrics/search?track_name=Viva+La+Vida&artist_name=Coldplay&include_rich_sync=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	if upstreamSearches.Load() == 0 {
		t.Fatal("expected upstream search to be called even when artist has other cached songs")
	}

	var results []lyricsResponse
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode search response: %v", err)
	}

	found := false
	for _, r := range results {
		if r.TrackName == "Viva La Vida" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'Viva La Vida' in search results, got %+v", results)
	}
}

// TestSearchUpstreamNotCutOffByFastLocalResults verifies that an upstream provider
// taking 250ms (slower than local DB lookup <1ms) is NOT cut off by a grace timer.
func TestSearchUpstreamNotCutOffByFastLocalResults(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)

	// Seed 1 existing local track that partially matches the query
	_, _, err := db.InsertTrackWithLyrics(context.Background(), metadataDB, lyricsDB, db.Track{
		Name:       "Something",
		ArtistName: "The Beatles",
		AlbumName:  "Abbey Road",
		Duration:   182,
	}, db.Lyrics{
		PlainLyrics:  "something in the way she moves",
		SyncedLyrics: "[00:01.00]something in the way she moves",
	})
	if err != nil {
		t.Fatalf("seed track: %v", err)
	}

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" {
			upstreamCalls.Add(1)
			// Slower than 100ms
			time.Sleep(200 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":888,"trackName":"Come Together","artistName":"The Beatles","albumName":"Abbey Road","duration":259,"instrumental":false,"plainLyrics":"here come old flat top","syncedLyrics":"[00:01.00]here come old flat top"}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	cfg := fallbackConfig(upstream.URL + "/api")
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	// Free-text search matching "Beatles"
	resp := performRequest(t, server.Handler, "/api/lyrics/search?q=Beatles")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var results []lyricsResponse
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	foundComeTogether := false
	for _, r := range results {
		if r.TrackName == "Come Together" {
			foundComeTogether = true
			break
		}
	}
	if !foundComeTogether {
		t.Fatalf("expected upstream result 'Come Together' to be included, results=%+v", results)
	}
}

// TestParallelLookupBudgetConservesTokens verifies that querying multiple songs
// by the same artist consumes exactly 1 token per song from the fallback budget,
// rather than 11 tokens per song.
func TestParallelLookupBudgetConservesTokens(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/get" {
			upstreamCalls.Add(1)
			trackName := r.URL.Query().Get("track_name")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           100,
				"trackName":    trackName,
				"artistName":   "Artist",
				"albumName":    "Album",
				"duration":     200,
				"plainLyrics":  "plain",
				"syncedLyrics": "[00:01.00]synced",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	cfg := fallbackConfig(upstream.URL + "/api")
	// Set budget to 5 per minute. If each request consumed 11 tokens, the second request would 429.
	// With 1 token per request, 5 requests will succeed!
	cfg.FallbackPerMin = 5
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	for i := 1; i <= 5; i++ {
		songTitle := "Song" + string(rune('A'+i-1))
		resp := performRequest(t, server.Handler, "/api/lyrics/get?track_name="+songTitle+"&artist_name=Artist")
		if resp.Code != http.StatusOK {
			t.Fatalf("request %d (%s) failed with status %d (expected 200): %s", i, songTitle, resp.Code, resp.Body.String())
		}
	}

	// 6th request should hit the 429 rate limit
	resp6 := performRequest(t, server.Handler, "/api/lyrics/get?track_name=SongF&artist_name=Artist")
	if resp6.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 6th request to be rate limited (429), got %d: %s", resp6.Code, resp6.Body.String())
	}
}

// TestFallbackLyricsIdentityNoCrossContamination verifies that when artist_name is
// supplied but album_name is empty, fallbackLyricsIdentity does NOT backfill an
// unrelated album from another artist with a matching song title.
func TestFallbackLyricsIdentityNoCrossContamination(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)

	// Seed track "Intro" by "The xx" from album "xx"
	_, _, err := db.InsertTrackWithLyrics(context.Background(), metadataDB, lyricsDB, db.Track{
		Name:       "Intro",
		ArtistName: "The xx",
		AlbumName:  "xx",
		Duration:   127,
	}, db.Lyrics{
		PlainLyrics:  "instrumental",
		Instrumental: true,
	})
	if err != nil {
		t.Fatalf("seed track: %v", err)
	}

	var upstreamQueryAlbum string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/get" {
			upstreamQueryAlbum = r.URL.Query().Get("album_name")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":200,"trackName":"Intro","artistName":"Another Artist","albumName":"Another Album","duration":100,"plainLyrics":"words"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	cfg := fallbackConfig(upstream.URL + "/api")
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	// Request "Intro" by "Another Artist" with NO album specified
	resp := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Intro&artist_name=Another+Artist")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	if upstreamQueryAlbum == "xx" {
		t.Fatalf("upstream was queried with album 'xx' from 'The xx', cross-contaminating Another Artist!")
	}
}

// TestCandidateIterationResolvesHyphenatedTrackName verifies that a trackName in "Title - Artist"
// format correctly resolves when candidates are iterated.
func TestCandidateIterationResolvesHyphenatedTrackName(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)

	// Seed track where Name is "Title" and ArtistName is "Artist"
	_, _, err := db.InsertTrackWithLyrics(context.Background(), metadataDB, lyricsDB, db.Track{
		Name:       "Midnight City",
		ArtistName: "M83",
		AlbumName:  "Hurry Up, We're Dreaming",
		Duration:   243,
	}, db.Lyrics{
		PlainLyrics:  "waiting in a car",
		SyncedLyrics: "[00:01.00]waiting in a car",
	})
	if err != nil {
		t.Fatalf("seed track: %v", err)
	}

	cfg := config.Config{Port: "8080"}
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	// Lookup using single track_name parameter with hyphen: "M83 - Midnight City"
	resp1 := performRequest(t, server.Handler, "/api/lyrics/get?track_name=M83+-+Midnight+City")
	if resp1.Code != http.StatusOK {
		t.Fatalf("expected 200 for 'M83 - Midnight City', got %d: %s", resp1.Code, resp1.Body.String())
	}

	// Lookup using single track_name parameter with inverted hyphen: "Midnight City - M83"
	resp2 := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Midnight+City+-+M83")
	if resp2.Code != http.StatusOK {
		t.Fatalf("expected 200 for 'Midnight City - M83', got %d: %s", resp2.Code, resp2.Body.String())
	}
}
