package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
)

// TestEntityCoverPositiveCacheServesWithoutUpstream verifies the live
// artist/album route serves a cached cover immediately and never consults a
// provider for it.
func TestEntityCoverPositiveCacheServesWithoutUpstream(t *testing.T) {
	database := testCoverDB(t)
	if err := db.UpsertCoverArt(context.Background(), database, db.CoverArtist, "Radiohead", "", "http://cached/cover.jpg", "itunes"); err != nil {
		t.Fatalf("seed cover: %v", err)
	}

	stub := &coverStubProvider{name: "itunes", result: &cover.Result{URL: "http://fresh/cover.jpg", Source: "itunes"}}
	handler := getEntityCoverSearchHandler(database, cover.NewResolver(stub), testFallbackGuard(), db.CoverArtist, true)

	first := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if first.Code != http.StatusOK {
		t.Fatalf("expected cached 200, got %d: %s", first.Code, first.Body.String())
	}
	var got albumArtistCoverResponse
	if err := json.NewDecoder(first.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CoverURL != "http://cached/cover.jpg" || got.CoverSource != "itunes" {
		t.Fatalf("expected cached cover, got %+v", got)
	}
	// A cache hit still exposes the cached URL through results.
	if len(got.Results) != 1 || got.Results[0].CoverURL != "http://cached/cover.jpg" {
		t.Fatalf("expected the cached URL in results on a hit, got %+v", got.Results)
	}
	// Positive cache hit must not spend upstream budget.
	if stub.calls != 0 {
		t.Fatalf("expected no upstream call on a positive cache hit, got %d", stub.calls)
	}
}

// TestEntityCoverMissResolvesUpstream verifies a provider consult happens only
// on a genuine miss, not on every request.
func TestEntityCoverMissResolvesUpstream(t *testing.T) {
	database := testCoverDB(t)
	stub := &coverStubProvider{name: "itunes", result: &cover.Result{URL: "http://resolved/cover.jpg", Source: "itunes"}}
	handler := getEntityCoverSearchHandler(database, cover.NewResolver(stub), testFallbackGuard(), db.CoverArtist, true)

	response := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if response.Code != http.StatusOK {
		t.Fatalf("expected resolved 200, got %d: %s", response.Code, response.Body.String())
	}
	var got albumArtistCoverResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CoverURL != "http://resolved/cover.jpg" {
		t.Fatalf("expected resolved URL, got %q", got.CoverURL)
	}

	// The second request should now be served from the cache the first miss
	// stored, with no further upstream call.
	second := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if second.Code != http.StatusOK {
		t.Fatalf("expected cached 200 on second request, got %d", second.Code)
	}
	if stub.calls != 1 {
		t.Fatalf("expected a single upstream consult then cache, got %d calls", stub.calls)
	}
}

// TestEntityCoverMissStoresAllVariants verifies every plausible provider URL is
// persisted on a miss and served from the cache on later hits, not just the
// winner.
func TestEntityCoverMissStoresAllVariants(t *testing.T) {
	database := testCoverDB(t)
	lastfm := &coverStubProvider{name: "lastfm", result: &cover.Result{URL: "http://img/lastfm.jpg", Source: "lastfm", ArtistName: "Radiohead"}}
	itunes := &coverStubProvider{name: "itunes", result: &cover.Result{URL: "http://img/itunes.jpg", Source: "itunes", ArtistName: "Radiohead"}}
	handler := getEntityCoverSearchHandler(database, cover.NewResolver(lastfm, itunes), testFallbackGuard(), db.CoverArtist, true)

	response := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if response.Code != http.StatusOK {
		t.Fatalf("expected resolved 200, got %d: %s", response.Code, response.Body.String())
	}
	var got albumArtistCoverResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CoverURL != "http://img/lastfm.jpg" {
		t.Fatalf("expected the first provider to win, got %q", got.CoverURL)
	}
	if len(got.Results) != 2 {
		t.Fatalf("expected both provider URLs in results, got %+v", got.Results)
	}

	// A second request is served from the cache and still returns both URLs.
	second := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if second.Code != http.StatusOK {
		t.Fatalf("expected cached 200 on second request, got %d", second.Code)
	}
	var cached albumArtistCoverResponse
	if err := json.NewDecoder(second.Body).Decode(&cached); err != nil {
		t.Fatalf("decode cached: %v", err)
	}
	if len(cached.Results) != 2 || cached.Results[1].CoverURL != "http://img/itunes.jpg" {
		t.Fatalf("expected both cached URLs in the hit results, got %+v", cached.Results)
	}
}

// TestEntityCoverFreshNegativeMissIsCached verifies a fresh negative result is
// served as 404 without re-consulting upstream.
func TestEntityCoverFreshNegativeMissIsCached(t *testing.T) {
	database := testCoverDB(t)
	// Seed a recent negative result (empty URL, checked now).
	if err := db.UpsertCoverArt(context.Background(), database, db.CoverAlbum, "Radiohead", "OK Computer", "", ""); err != nil {
		t.Fatalf("seed negative: %v", err)
	}

	stub := &coverStubProvider{name: "lastfm"}
	handler := getEntityCoverSearchHandler(database, cover.NewResolver(stub), testFallbackGuard(), db.CoverAlbum, true)

	response := performArtistRequest(t, handler, "/?artist_name=Radiohead&album_name=OK+Computer")
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected negative-cache 404, got %d", response.Code)
	}
	if stub.calls != 0 {
		t.Fatalf("expected no upstream call on a fresh negative miss, got %d", stub.calls)
	}
}
