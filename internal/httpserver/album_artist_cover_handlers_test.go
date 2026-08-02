package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
	}), true)

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
	handler = getArtistCoverHandler(database, cover.NewResolver(stub), true)
	second := performRequest(t, handler, "/?artist_name=Radiohead")
	if second.Code != http.StatusOK {
		t.Fatalf("expected cached 200, got %d", second.Code)
	}
	// Second hit is served locally, so the resolver is only consulted once.
	if stub.calls != 0 {
		t.Fatalf("expected cached local hit without upstream, got %d calls", stub.calls)
	}
}

func TestAlbumCoverActorCache(t *testing.T) {
	database := testCoverDB(t)
	stub := &coverStubProvider{name: "lastfm"}
	handler := getAlbumCoverHandler(database, cover.NewResolver(stub), true)

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

func TestAlbumCoverRequiresArtistAndAlbum(t *testing.T) {
	db := testCoverDB(t)
	handler := getAlbumCoverHandler(db, nil, true)
	first := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if first.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without album_name, got %d", first.Code)
	}
	second := performArtistRequest(t, handler, "/?album_name=OK+Computer")
	if second.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without artist_name, got %d", second.Code)
	}
}

func TestArtistCoverDisabledFallbackServesNotFound(t *testing.T) {
	db := testCoverDB(t)
	handler := getArtistCoverHandler(db, cover.NewResolver(&coverStubProvider{name: "lastfm"}), false)
	response := performArtistRequest(t, handler, "/?artist_name=Radiohead")
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when fallback disabled, got %d", response.Code)
	}
}

func performArtistRequest(t *testing.T, handler http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
