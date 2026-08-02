package cover

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestITunesArtistImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("entity"); got != "album" {
			t.Fatalf("unexpected entity: %q", got)
		}
		if got := r.URL.Query().Get("term"); got != "Radiohead" {
			t.Fatalf("unexpected term: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultCount":1,"results":[{"collectionName":"OK Computer","artworkUrl100":"http://img/100x100.jpg"}]}`))
	}))
	defer server.Close()

	client, err := NewITunes(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.Lookup(context.Background(), Artist, Input{ArtistName: "Radiohead"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "http://img/600x600.jpg" || result.Source != "itunes" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestITunesAlbumQueryConcatenatedTerm(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("term"); got != "Radiohead OK Computer" {
			t.Fatalf("unexpected term: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultCount":1,"results":[{"collectionName":"OK Computer","artworkUrl100":"http://img/100x100.jpg"}]}`))
	}))
	defer server.Close()

	client, err := NewITunes(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.Lookup(context.Background(), Album, Input{ArtistName: "Radiohead", AlbumName: "OK Computer"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "http://img/600x600.jpg" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestDeezerArtistImagePrecedence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/artist" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"picture_xl":"http://img/xl.jpg","picture_big":"http://img/big.jpg","picture_medium":"http://img/med.jpg"}]}`))
	}))
	defer server.Close()

	client, err := NewDeezer(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.Lookup(context.Background(), Artist, Input{ArtistName: "Radiohead"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "http://img/xl.jpg" || result.Source != "deezer" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestDeezerAlbumCoverPrecedence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/album" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); !strings.Contains(got, "Radiohead") || !strings.Contains(got, "OK Computer") {
			t.Fatalf("unexpected query: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"cover_xl":"http://img/xl.jpg"}]}`))
	}))
	defer server.Close()

	client, err := NewDeezer(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.Lookup(context.Background(), Album, Input{ArtistName: "Radiohead", AlbumName: "OK Computer"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "http://img/xl.jpg" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestLastfmtScrapesGIF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/music/Radiohead/+images" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`<img src="https://lastfm.fastly.net/i/u/avatar170s/abc.jpg" alt="gif">`))
	}))
	defer server.Close()

	client, _ := NewLastfm(server.URL, "test-agent", time.Second)
	result, err := client.Lookup(context.Background(), Artist, Input{ArtistName: "Radiohead"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "https://lastfm.fastly.net/i/u/300x300/abc.jpg" || result.Source != "lastfm" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestLastfmtFallsBackToArtistPage(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/music/Radiohead/+images" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/music/Radiohead" {
			_, _ = w.Write([]byte(`<meta property="og:image" content="http://lastfm.fastly.net/i/u/174s/def.jpg">`))
			return
		}
		t.Fatalf("unexpected path: %s", r.URL.Path)
	}))
	defer server.Close()

	client, _ := NewLastfm(server.URL, "test-agent", time.Second)
	result, err := client.Lookup(context.Background(), Artist, Input{ArtistName: "Radiohead"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "http://last.test.fastly.net/i/u/300x300/def.jpg" && !strings.Contains(result.URL, "300x300") {
		t.Fatalf("unexpected result: %+v", result)
	}
	if calls != 2 {
		t.Fatalf("expected 2 page fetches, got %d", calls)
	}
}

func TestLastfmRejectsDummyHash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<img src="http://last.fastly.net/i/u/300x300/2a9cbd8b46e8849cf0f1c91b013a0edf.jpg">`))
	}))
	defer server.Close()

	client, _ := NewLastfm(server.URL, "test-agent", time.Second)
	if _, err := client.Lookup(context.Background(), Artist, Input{ArtistName: "Radiohead"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCleanTagRejectsSentinels(t *testing.T) {
	for _, value := range []string{"unknown", "unknown artist", "unknown album", "  Unknown Title  "} {
		if got := CleanArtist(value); got != "" {
			t.Fatalf("CleanArtist(%q) = %q, want empty", value, got)
		}
	}
	if got := CleanArtist("  Radiohead "); got != "Radiohead" {
		t.Fatalf("CleanArtist = %q, want Radiohead", got)
	}
}

func TestResolverChainsInOrder(t *testing.T) {
	first := &stubProvider{name: "first", err: ErrNotFound}
	second := &stubProvider{name: "second", result: &Result{URL: "http://second/cover.jpg", Source: "second"}}
	never := &stubProvider{name: "never"}
	r := NewResolver(first, second, never)

	result, err := r.Lookup(context.Background(), Artist, Input{ArtistName: "Radiohead"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "http://second/cover.jpg" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if first.calls != 1 || second.calls != 1 || never.calls != 0 {
		t.Fatalf("unexpected call counts: first=%d second=%d never=%d", first.calls, second.calls, never.calls)
	}
}

func TestResolverNegativeCache(t *testing.T) {
	p := &stubProvider{name: "p", err: ErrNotFound}
	r := NewResolver(p)

	for i := 0; i < 3; i++ {
		if _, err := r.Lookup(context.Background(), Artist, Input{ArtistName: "Ghost"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	}
	if p.calls != 1 {
		t.Fatalf("expected one upstream call after negative caching, got %d", p.calls)
	}
}

type stubProvider struct {
	name   string
	result *Result
	err    error
	calls  int
}

func (p *stubProvider) Name() string { return p.name }
func (p *stubProvider) Lookup(_ context.Context, _ Kind, _ Input) (*Result, error) {
	p.calls++
	return p.result, p.err
}
