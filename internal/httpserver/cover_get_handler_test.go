package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
)

// coverDisabledFallbackConfig returns a server config with every upstream
// fallback disabled so handler tests exercise local-only behavior without
// touching real providers.
func coverDisabledFallbackConfig() config.Config {
	return config.Config{
		Port:                    "8080",
		RateLimitPerSec:         1000,
		RateLimitPerMin:         100000,
		MetadataFallbackEnabled: false,
		CoverFallbackEnabled:    false,
		LRCLIBFallbackEnabled:   false,
	}
}

func TestCoverGetSongArtistOptional(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(coverDisabledFallbackConfig(), metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	// An artist-less song cover is a miss, not a validation error, when there
	// is no fallback to resolve it.
	response := performRequest(t, server.Handler, "/api/cover/get?track_name=Example+Song")
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected artist-less song cover to be a miss (404), got %d: %s", response.Code, response.Body.String())
	}

	// A missing track_name is still a validation error.
	response = performRequest(t, server.Handler, "/api/cover/get?artist_name=Example+Artist")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected missing track_name to be 400, got %d", response.Code)
	}
}

func TestCoverGetArtistTypeRequiresArtist(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(coverDisabledFallbackConfig(), metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/cover/get?type=artist&track_name=Example+Song")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected artist type to require artist_name (400), got %d", response.Code)
	}
}

func TestCoverAlbumArtistOptional(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(coverDisabledFallbackConfig(), metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	// An artist-less album cover is a miss, not a validation error, when there
	// is no fallback to resolve it.
	response := performRequest(t, server.Handler, "/api/cover/album?album_name=OK+Computer")
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected artist-less album cover to be a miss (404), got %d: %s", response.Code, response.Body.String())
	}

	// A missing album_name is still a validation error.
	response = performRequest(t, server.Handler, "/api/cover/album?artist_name=Radiohead")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected missing album_name to be 400, got %d", response.Code)
	}

	// Artist covers still require an artist.
	response = performRequest(t, server.Handler, "/api/cover/artist?track_name=Example")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected artist cover to require artist_name (400), got %d", response.Code)
	}
}

// TestCoverGetSongTitleOnlyResolves proves an artist-less song cover resolves
// through the provider chain rather than failing as a validation error.
func TestCoverGetSongTitleOnlyResolves(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	handler := getCoverTopHandler(metadataDB, nil, cover.NewResolver(&coverStubProvider{
		name:   "itunes",
		result: &cover.Result{URL: "http://img/song.jpg", Source: "itunes", TrackName: "Example Song", ArtistName: "Example Artist", AlbumName: "Example Album"},
	}), testFallbackGuard(), true)

	response := performRequest(t, handler, "/?track_name=Example+Song")
	if response.Code != http.StatusOK {
		t.Fatalf("expected title-only song cover to resolve (200), got %d: %s", response.Code, response.Body.String())
	}
	var got coverSearchResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CoverURL != "http://img/song.jpg" || got.CoverSource != "itunes" {
		t.Fatalf("unexpected cover response: %+v", got)
	}
}

// metadataStubProvider is a metadata provider stub for handler tests.
type metadataStubProvider struct {
	name   string
	tracks []*db.Track
}

func (p *metadataStubProvider) Name() string { return p.name }
func (p *metadataStubProvider) Lookup(_ context.Context, _ metadata.Input) (*db.Track, error) {
	if len(p.tracks) == 0 {
		return nil, metadata.ErrNotFound
	}
	return p.tracks[0], nil
}
func (p *metadataStubProvider) Search(_ context.Context, _ string, limit int) ([]*db.Track, error) {
	if limit < 1 {
		return []*db.Track{}, nil
	}
	if limit > len(p.tracks) {
		limit = len(p.tracks)
	}
	return p.tracks[:limit], nil
}

func TestCoverSearchFreeTextMixed(t *testing.T) {
	metadataResolver := metadata.NewResolver(&metadataStubProvider{
		name: "itunes",
		tracks: []*db.Track{{
			Name: "Example Song", ArtistName: "Example Artist", AlbumName: "Example Album",
			CoverURL: "http://img/song.jpg", CoverURLSource: "itunes",
		}},
	})
	coverResolver := cover.NewResolver(&coverStubProvider{
		name:   "itunes",
		result: &cover.Result{URL: "http://img/cover.jpg", Source: "itunes", ArtistName: "Example Artist", AlbumName: "Example Album"},
	})
	handler := searchCoverHandler(metadataResolver, coverResolver, testFallbackGuard(), true)

	response := performRequest(t, handler, "/?q=example&limit=10")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []coverSearchResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected song + album + artist results, got %d: %+v", len(results), results)
	}
	// Round-robin merge keeps one result per type at the front.
	for i, entity := range []string{"song", "album", "artist"} {
		if results[i].EntityType != entity {
			t.Fatalf("result %d entityType = %q, want %q", i, results[i].EntityType, entity)
		}
	}

	// Structured per-type search still works when q is omitted.
	structured := performRequest(t, handler, "/?type=song&track_name=Example+Song")
	if structured.Code != http.StatusOK {
		t.Fatalf("expected structured 200, got %d: %s", structured.Code, structured.Body.String())
	}
	var got []coverSearchResponse
	if err := json.NewDecoder(structured.Body).Decode(&got); err != nil {
		t.Fatalf("decode structured: %v", err)
	}
	if len(got) != 1 || got[0].EntityType != "song" {
		t.Fatalf("unexpected structured results: %+v", got)
	}
}

func TestCoverSearchFreeTextNarrowedByType(t *testing.T) {
	metadataResolver := metadata.NewResolver(&metadataStubProvider{
		name: "itunes",
		tracks: []*db.Track{{Name: "Example Song", ArtistName: "Example Artist", CoverURL: "http://img/song.jpg", CoverURLSource: "itunes"}},
	})
	coverResolver := cover.NewResolver(&coverStubProvider{
		name:   "itunes",
		result: &cover.Result{URL: "http://img/artist.jpg", Source: "itunes", ArtistName: "Example Artist"},
	})
	handler := searchCoverHandler(metadataResolver, coverResolver, testFallbackGuard(), true)

	response := performRequest(t, handler, "/?q=example&type=artist&limit=10")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []coverSearchResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].EntityType != "artist" || results[0].CoverURL != "http://img/artist.jpg" {
		t.Fatalf("unexpected narrowed results: %+v", results)
	}
}

func TestMergeCoverSearch(t *testing.T) {
	item := func(entity, album string) coverSearchResponse {
		return coverSearchResponse{EntityType: entity, ArtistName: "a", AlbumName: album}
	}
	merged := mergeCoverSearch(10,
		[]coverSearchResponse{item("song", "b1"), item("song", "b2"), item("song", "b3")},
		[]coverSearchResponse{item("album", "b1"), item("album", "b2")},
		[]coverSearchResponse{item("artist", "b1")},
	)
	want := []string{"song", "album", "artist", "song", "album", "song"}
	if len(merged) != len(want) {
		t.Fatalf("expected %d merged results, got %d: %+v", len(want), len(merged), merged)
	}
	for i, entity := range want {
		if merged[i].EntityType != entity {
			t.Fatalf("merged[%d] = %q, want %q", i, merged[i].EntityType, entity)
		}
	}

	// limit caps the merged array.
	limited := mergeCoverSearch(2, []coverSearchResponse{item("song", "b1"), item("song", "b2")}, []coverSearchResponse{item("album", "b1")})
	if len(limited) != 2 {
		t.Fatalf("expected limit 2, got %d", len(limited))
	}
}
