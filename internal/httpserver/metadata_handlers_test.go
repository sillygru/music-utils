package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
)

func TestGetMetadataArtistOptional(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	server := New("8080", metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	response := performRequest(t, server.Handler, "/api/metadata/get?track_name=Example+Artist+-+Example+Song+(Official+Music+Video).mp3")
	if response.Code != http.StatusOK {
		t.Fatalf("expected artist-less metadata lookup to return 200, got %d: %s", response.Code, response.Body.String())
	}
	var got metadataResponse
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.TrackName != "Example Song" || got.ArtistName != "Example Artist" || got.AlbumName != "Example Album" {
		t.Fatalf("unexpected artist-less metadata result: %+v", got)
	}

	missingTrack := performRequest(t, server.Handler, "/api/metadata/get?artist_name=Example+Artist")
	if missingTrack.Code != http.StatusBadRequest {
		t.Fatalf("expected missing track_name to be 400, got %d", missingTrack.Code)
	}
}

func TestSearchMetadataPrefersLocalExactMatchOverProviderPage(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
		Name:       "Witches Burn",
		ArtistName: "The Pretty Reckless",
		AlbumName:  "Death by Rock and Roll",
		Duration:   294,
	}); err != nil {
		t.Fatalf("seed track: %v", err)
	}

	// A provider page of the same artist that omits the requested title, the
	// shape iTunes returns for a term it cannot match exactly.
	decoys := []*db.Track{
		{Name: "Make Me Wanna Die", ArtistName: "The Pretty Reckless", AlbumName: "Light Me Up"},
		{Name: "Burn", ArtistName: "The Pretty Reckless", AlbumName: "Going to Hell (Deluxe Edition)"},
	}
	provider := &metadataStubProvider{name: "itunes", tracks: decoys}
	resolver := metadata.NewResolver(provider)
	handler := searchMetadataHandlerWithUpstream(metadataDB, resolver, testFallbackGuard(), true)

	response := performRequest(t, handler, "/api/metadata/search?q=The+Pretty+Reckless+Witches+Burn&limit=12")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []metadataResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// A search that already has a local match answers from it rather than
	// waiting on a provider that can only add loosely related rows.
	if len(results) != 1 || results[0].TrackName != "Witches Burn" {
		t.Fatalf("expected the local exact match alone, got %d: %+v", len(results), results)
	}

	// The provider is still asked, and its answers are persisted so the next
	// identical search is served entirely from the local database.
	if err := waitForMetadataSearchFill(t, metadataDB, "Make Me Wanna Die"); err != nil {
		t.Fatal(err)
	}
	// A query both the seeded row and the persisted provider row satisfy. The
	// provider row is only reachable now because the background fill stored it.
	second := performRequest(t, handler, "/api/metadata/search?q=The+Pretty+Reckless+Burn&limit=12")
	if second.Code != http.StatusOK {
		t.Fatalf("expected 200 on the repeat search, got %d: %s", second.Code, second.Body.String())
	}
	var filled []metadataResponse
	if err := json.NewDecoder(second.Body).Decode(&filled); err != nil {
		t.Fatalf("decode repeat response: %v", err)
	}
	if len(filled) != 2 {
		t.Fatalf("expected the local match plus the persisted provider hit, got %d: %+v", len(filled), filled)
	}
	// The local exact match keeps its rank ahead of the provider's fuzzy hits.
	if filled[0].TrackName != "Witches Burn" || filled[1].TrackName != "Burn" {
		t.Fatalf("results missing or reordered: %+v", filled)
	}
}

// waitForMetadataSearchFill blocks until the detached background fan-out has
// persisted a track by the given name, so a test never races the goroutine that
// makes the second request fast.
func waitForMetadataSearchFill(t *testing.T, database *sql.DB, name string) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tracks, err := db.SearchTracks(context.Background(), database, nil, name, 1)
		if err == nil && len(tracks) > 0 {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("background metadata search fill did not persist %q", name)
}

// A search with nothing in the local database has no early answer to give, so it
// waits for the fan-out rather than returning an empty page.
func TestSearchMetadataColdQueryWaitsForUpstream(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	provider := &metadataStubProvider{name: "itunes", tracks: []*db.Track{
		{Name: "Only Upstream", ArtistName: "Nobody Cached", AlbumName: "Cold"},
	}}
	handler := searchMetadataHandlerWithUpstream(metadataDB, metadata.NewResolver(provider), testFallbackGuard(), true)

	response := performRequest(t, handler, "/api/metadata/search?q=Only+Upstream&limit=10")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []metadataResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(results) != 1 || results[0].TrackName != "Only Upstream" {
		t.Fatalf("expected the upstream hit on a cold query, got %d: %+v", len(results), results)
	}
	// The synchronous path persists too, so the repeat query needs no upstream.
	if err := waitForMetadataSearchFill(t, metadataDB, "Only Upstream"); err != nil {
		t.Fatal(err)
	}
}

// A full local page is the complete answer, so the provider is not asked at all.
func TestSearchMetadataFullLocalPageSkipsUpstream(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	for _, name := range []string{"Local One", "Local Two"} {
		if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
			Name: name, ArtistName: "Cached Artist", Duration: 100,
		}); err != nil {
			t.Fatalf("seed track %q: %v", name, err)
		}
	}
	provider := &countingMetadataProvider{}
	handler := searchMetadataHandlerWithUpstream(metadataDB, metadata.NewResolver(provider), testFallbackGuard(), true)

	response := performRequest(t, handler, "/api/metadata/search?q=Local&limit=2")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []metadataResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected both local rows, got %d: %+v", len(results), results)
	}
	// Give a background fan-out the chance to run before asserting it never
	// started; the point is that it was never even queued.
	time.Sleep(50 * time.Millisecond)
	if calls := provider.searchCalls(); calls != 0 {
		t.Fatalf("provider was called %d time(s) for a full local page, want 0", calls)
	}
}

// countingMetadataProvider records how often a search reached it.
type countingMetadataProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *countingMetadataProvider) Name() string { return "counting" }

func (p *countingMetadataProvider) Lookup(_ context.Context, _ metadata.Input) (*db.Track, error) {
	return nil, metadata.ErrNotFound
}

func (p *countingMetadataProvider) Search(_ context.Context, _ string, _ int) ([]*db.Track, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return []*db.Track{}, nil
}

func (p *countingMetadataProvider) searchCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// The title track of an album lands in the catalog after its siblings, so an
// id-ordered search returned every other song on the record first and pushed it
// off the page entirely.
func TestSearchMetadataRanksTheTitleTrackFirst(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	for _, name := range []string{"Rollercoaster Of Life", "Eye Of The Storm", "Life Evermore Pt. 2", "Life Evermore Pt. 3", "Dark Days", "Dragonfire", "Love Me", "Spell On You", "About You", "When I Wake Up", "For I Am Death", "Devil In Disguise"} {
		if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
			Name: name, ArtistName: "The Pretty Reckless", AlbumName: "Dear God",
			Duration: 240, Genre: "Hard Rock", Year: 2026, CoverURL: "https://example.test/cover.jpg",
		}); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}
	if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
		Name: "Dear God", ArtistName: "The Pretty Reckless", AlbumName: "Dear God",
		Duration: 368, Genre: "Hard Rock", Year: 2026, CoverURL: "https://example.test/cover.jpg",
	}); err != nil {
		t.Fatalf("seed title track: %v", err)
	}
	handler := searchMetadataHandlerWithUpstream(metadataDB, nil, testFallbackGuard(), false)

	response := performRequest(t, handler, "/api/metadata/search?q=Dear+God+The+Pretty+Reckless&limit=10")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []metadataResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(results) != 10 {
		t.Fatalf("expected a full page of 10, got %d", len(results))
	}
	if results[0].TrackName != "Dear God" {
		t.Fatalf("expected Dear God first, got %q", results[0].TrackName)
	}
}

// A query with a word the catalog does not hold is answered from the cache
// rather than costing a provider round trip.
func TestSearchMetadataRelaxedPassAnswersMisspelledQueriesLocally(t *testing.T) {
	metadataDB, _ := testHTTPDatabases(t)
	if _, err := db.UpsertTrackMetadata(context.Background(), metadataDB, db.Track{
		Name: "Dear God", ArtistName: "The Pretty Reckless", AlbumName: "Dear God",
		Duration: 368, Genre: "Rock", Year: 2026,
	}); err != nil {
		t.Fatalf("seed track: %v", err)
	}
	// Providers are disabled outright: the only way this returns the track is
	// if the relaxed pass found it in the catalog.
	handler := searchMetadataHandlerWithUpstream(metadataDB, nil, testFallbackGuard(), false)

	response := performRequest(t, handler, "/api/metadata/search?q=Dear+Gid+The+Pretty+Reckless&limit=10")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var results []metadataResponse
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(results) != 1 || results[0].TrackName != "Dear God" {
		t.Fatalf("expected the cached hit, got %+v", results)
	}
}
