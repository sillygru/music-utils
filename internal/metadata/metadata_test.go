package metadata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

func TestITunesLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != "test-agent" {
			t.Fatalf("unexpected user agent: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultCount":1,"results":[{"trackName":"Blinding Lights","artistName":"The Weeknd","collectionName":"After Hours","trackTimeMillis":200000,"releaseDate":"2020-03-20T08:00:00Z","primaryGenreName":"R&B/Soul","artworkUrl600":"http://img/itunes/600x600.jpg","artworkUrl100":"http://img/itunes/100x100.jpg"}]}`))
	}))
	defer server.Close()

	client, err := NewITunes(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	track, err := client.Lookup(context.Background(), Input{TrackName: "Blinding Lights", ArtistName: "The Weeknd"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if track.Name != "Blinding Lights" || track.ArtistName != "The Weeknd" || track.AlbumName != "After Hours" {
		t.Fatalf("unexpected track: %+v", track)
	}
	if track.Duration != 200 {
		t.Fatalf("unexpected duration: %v", track.Duration)
	}
	if track.CoverURL != "http://img/itunes/600x600.jpg" || track.CoverURLSource != "itunes" {
		t.Fatalf("unexpected cover: %q %q", track.CoverURL, track.CoverURLSource)
	}
	if track.MetadataSource != "itunes" {
		t.Fatalf("unexpected source: %q", track.MetadataSource)
	}
}

func TestDeezerLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		query := r.URL.Query().Get("q")
		if !strings.Contains(query, `track:"Blinding Lights"`) || !strings.Contains(query, `artist:"The Weeknd"`) {
			t.Fatalf("unexpected query: %q", query)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":1,"title":"Blinding Lights","artist":{"name":"The Weeknd"},"album":{"title":"After Hours","cover_big":"http://img/deezer/500x500.jpg"},"duration":200,"isrc":"USUG11904251","release_date":"2020-03-20"}]}`))
	}))
	defer server.Close()

	client, err := NewDeezer(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	track, err := client.Lookup(context.Background(), Input{TrackName: "Blinding Lights", ArtistName: "The Weeknd"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if track.AlbumName != "After Hours" || track.ISRC != "USUG11904251" {
		t.Fatalf("unexpected track: %+v", track)
	}
	if track.CoverURL != "http://img/deezer/500x500.jpg" || track.CoverURLSource != "deezer" {
		t.Fatalf("unexpected cover: %q %q", track.CoverURL, track.CoverURLSource)
	}
	if track.MetadataSource != "deezer" {
		t.Fatalf("unexpected source: %q", track.MetadataSource)
	}
}

type stubProvider struct {
	name  string
	track *db.Track
	err   error
	call  atomic.Int32
}

func (p *stubProvider) Name() string { return p.name }
func (p *stubProvider) Lookup(_ context.Context, _ Input) (*db.Track, error) {
	p.call.Add(1)
	return p.track, p.err
}

func TestResolverChainsToFallback(t *testing.T) {
	primary := &stubProvider{name: "primary", err: ErrNotFound}
	secondary := &stubProvider{name: "secondary", track: &db.Track{Name: "Found", ArtistName: "Artist"}}
	never := &stubProvider{name: "never"}
	r := NewResolver(primary, secondary, never)

	track, err := r.Lookup(context.Background(), Input{TrackName: "Found", ArtistName: "Artist"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if track.Name != "Found" {
		t.Fatalf("unexpected track: %+v", track)
	}
	if primary.call.Load() != 1 || secondary.call.Load() != 1 || never.call.Load() != 0 {
		t.Fatalf("unexpected call counts: primary=%d secondary=%d never=%d", primary.call.Load(), secondary.call.Load(), never.call.Load())
	}
}

func TestResolverCachesPositiveResult(t *testing.T) {
	p := &stubProvider{name: "p", track: &db.Track{Name: "A", ArtistName: "B"}}
	r := NewResolver(p)

	for i := 0; i < 3; i++ {
		if _, err := r.Lookup(context.Background(), Input{TrackName: "A", ArtistName: "B"}); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}
	if p.call.Load() != 1 {
		t.Fatalf("expected one upstream call after caching, got %d", p.call.Load())
	}
}

func TestResolverNegativeCache(t *testing.T) {
	p := &stubProvider{name: "p", err: ErrNotFound}
	r := NewResolver(p)

	for i := 0; i < 3; i++ {
		if _, err := r.Lookup(context.Background(), Input{TrackName: "nope", ArtistName: "x"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	}
	if p.call.Load() != 1 {
		t.Fatalf("expected one upstream call after negative caching, got %d", p.call.Load())
	}
}

func TestResolverSkipsNilProviders(t *testing.T) {
	p := &stubProvider{name: "p", track: &db.Track{Name: "A", ArtistName: "B"}}
	r := NewResolver(nil, nil, p)

	if _, err := r.Lookup(context.Background(), Input{TrackName: "A", ArtistName: "B"}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
}