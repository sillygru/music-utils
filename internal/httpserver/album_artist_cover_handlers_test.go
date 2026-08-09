package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
)

type coverStubProvider struct {
	name   string
	result *cover.Result
	calls  int
}

func (c *coverStubProvider) Name() string { return c.name }
func (c *coverStubProvider) Lookup(_ context.Context, _ cover.Kind, _ cover.Input) (*cover.Result, error) {
	c.calls++
	if c.result == nil {
		return nil, cover.ErrNotFound
	}
	return c.result, nil
}

// coverSearchStub is a cover provider stub that returns multiple candidates so
// handler tests exercise the multi-result filter path.
type coverSearchStub struct {
	name    string
	results []cover.Result
	calls   int
}

func (c *coverSearchStub) Name() string { return c.name }
func (c *coverSearchStub) Lookup(_ context.Context, _ cover.Kind, _ cover.Input) (*cover.Result, error) {
	c.calls++
	if len(c.results) == 0 {
		return nil, cover.ErrNotFound
	}
	return &c.results[0], nil
}
func (c *coverSearchStub) Search(_ context.Context, _ cover.Kind, _ cover.Input, limit int) ([]cover.Result, error) {
	c.calls++
	out := make([]cover.Result, 0, len(c.results))
	for _, result := range c.results {
		if len(out) >= limit {
			break
		}
		out = append(out, result)
	}
	return out, nil
}

// testFallbackGuard returns a guard with generous limits so handler-level
// tests never trip the per-IP budget or the queue gate.
func testFallbackGuard() *fallbackGuard {
	return newFallbackGuard(config.Config{FallbackPerMin: 100, FallbackMaxQueue: 100})
}

func testCoverDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(":memory:", db.Config{MmapSize: 1024 * 1024, CacheSizeKB: -2, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateCover(context.Background(), database); err != nil {
		t.Fatalf("migrate cover database: %v", err)
	}
	return database
}

func TestArtistCoverProviderFallbackHitAndCaches(t *testing.T) {
	database := testCoverDB(t)
	handler := getArtistCoverHandler(database, cover.NewResolver(&coverStubProvider{
		name:   "itunes",
		result: &cover.Result{URL: "http://img/artist.jpg", Source: "itunes"},
	}), testFallbackGuard(), 0, true)

	first := performRequest(t, handler, "/?artist_name=Radiohead")
	if first.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", first.Code, first.Body.String())
	}
	var response albumArtistCoverResponse
	if err := json.NewDecoder(first.Body).Decode(&response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.CoverURL != "http://img/artist.jpg" || response.CoverSource != "itunes" {
		t.Fatalf("unexpected response: %+v", response)
	}

	stub := &coverStubProvider{name: "itunes", result: &cover.Result{URL: "http://img/artist.jpg", Source: "itunes"}}
	handler = getArtistCoverHandler(database, cover.NewResolver(stub), testFallbackGuard(), 0, true)
	second := performRequest(t, handler, "/?artist_name=Radiohead")
	if second.Code != http.StatusOK {
		t.Fatalf("expected cached 200, got %d", second.Code)
	}
	// Second hit is served locally, so the resolver is only consulted once.
	if stub.calls != 0 {
		t.Fatalf("expected cached local hit without upstream, got %d calls", stub.calls)
	}
}

func TestAlbumCoverTitleOnlyResolvesBestMatch(t *testing.T) {
	database := testCoverDB(t)
	stub := &coverSearchStub{name: "itunes", results: []cover.Result{
		{URL: "http://img/wrong.jpg", Source: "itunes", ArtistName: "NIFANA", AlbumName: "Imagine (Reggae Version) - Single"},
		{URL: "http://img/imagine.jpg", Source: "itunes", ArtistName: "John Lennon", AlbumName: "Imagine"},
	}}
	handler := getAlbumCoverHandler(database, cover.NewResolver(stub), testFallbackGuard(), 0, true)

	response := performRequest(t, handler, "/?album_name=Imagine")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var got albumArtistCoverResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CoverURL != "http://img/imagine.jpg" || got.CoverSource != "itunes" {
		t.Fatalf("expected the matching album cover, got %+v", got)
	}
	if len(got.Results) != 1 {
		t.Fatalf("expected 1 filtered result, got %d: %+v", len(got.Results), got.Results)
	}
}

func TestAlbumCoverActorCache(t *testing.T) {
	database := testCoverDB(t)
	stub := &coverStubProvider{name: "lastfm"}
	handler := getAlbumCoverHandler(database, cover.NewResolver(stub), testFallbackGuard(), 0, true)

	first := performRequest(t, handler, "/?artist_name=Radiohead&album_name=OK+Computer")
	if first.Code != http.StatusNotFound {
		t.Fatalf("expected 404 miss, got %d", first.Code)
	}
	second := performRequest(t, handler, "/?artist_name=Radiohead&album_name=OK+Computer")
	if second.Code != http.StatusNotFound {
		t.Fatalf("expected negative-cache 404, got %d", second.Code)
	}
	if stub.calls != 1 {
		t.Fatalf("expected resolver consulted once, got %d", stub.calls)
	}
}

func TestAlbumCoverRequiresAlbum(t *testing.T) {
	db := testCoverDB(t)
	handler := getAlbumCoverHandler(db, nil, testFallbackGuard(), 0, true)
	first := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if first.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without album_name, got %d", first.Code)
	}
	// artist_name is optional for album covers: an album-only request is a miss,
	// not a validation error.
	second := performArtistRequest(t, handler, "/?album_name=OK+Computer")
	if second.Code != http.StatusNotFound {
		t.Fatalf("expected album-only request to be a miss (404), got %d", second.Code)
	}
}

func TestArtistCoverDisabledFallbackServesNotFound(t *testing.T) {
	db := testCoverDB(t)
	handler := getArtistCoverHandler(db, cover.NewResolver(&coverStubProvider{name: "lastfm"}), testFallbackGuard(), 0, false)
	response := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when fallback disabled, got %d", response.Code)
	}
}

func TestArtistCoverStalePositiveReResolves(t *testing.T) {
	database := testCoverDB(t)
	if err := db.UpsertCoverArt(context.Background(), database, db.CoverArtist, "Radiohead", "", "http://old/cover.jpg", "itunes"); err != nil {
		t.Fatalf("seed cover: %v", err)
	}
	if _, err := database.ExecContext(context.Background(), `UPDATE cover_urls SET checked_at = datetime('now', '-40 days')`); err != nil {
		t.Fatalf("age cover row: %v", err)
	}

	stub := &coverStubProvider{name: "itunes", result: &cover.Result{URL: "http://fresh/cover.jpg", Source: "itunes"}}
	handler := getArtistCoverHandler(database, cover.NewResolver(stub), testFallbackGuard(), time.Hour, true)
	response := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if response.Code != http.StatusOK {
		t.Fatalf("expected re-resolved 200, got %d: %s", response.Code, response.Body.String())
	}
	var got albumArtistCoverResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CoverURL != "http://fresh/cover.jpg" {
		t.Fatalf("expected fresh URL, got %q", got.CoverURL)
	}
	if stub.calls != 1 {
		t.Fatalf("expected one re-resolution, got %d calls", stub.calls)
	}
}

func TestArtistCoverStalePositiveServedWhenFallbackDisabled(t *testing.T) {
	database := testCoverDB(t)
	if err := db.UpsertCoverArt(context.Background(), database, db.CoverArtist, "Radiohead", "", "http://old/cover.jpg", "itunes"); err != nil {
		t.Fatalf("seed cover: %v", err)
	}
	if _, err := database.ExecContext(context.Background(), `UPDATE cover_urls SET checked_at = datetime('now', '-40 days')`); err != nil {
		t.Fatalf("age cover row: %v", err)
	}

	handler := getArtistCoverHandler(database, cover.NewResolver(&coverStubProvider{name: "lastfm"}), testFallbackGuard(), time.Hour, false)
	response := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if response.Code != http.StatusOK {
		t.Fatalf("expected stale cached 200, got %d: %s", response.Code, response.Body.String())
	}
	var got albumArtistCoverResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CoverURL != "http://old/cover.jpg" {
		t.Fatalf("expected cached URL when fallback disabled, got %q", got.CoverURL)
	}
}

func performArtistRequest(t *testing.T, handler http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
