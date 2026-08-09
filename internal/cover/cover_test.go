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

	client, err := NewITunes(server.URL, "test-agent", time.Second, nil)
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

	client, err := NewITunes(server.URL, "test-agent", time.Second, nil)
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

func TestITunesSongTitleOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("entity"); got != "song" {
			t.Fatalf("unexpected entity: %q", got)
		}
		if got := r.URL.Query().Get("term"); got != "Hotel California" {
			t.Fatalf("expected title-only term without a leading artist, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultCount":1,"results":[{"trackName":"Hotel California","artistName":"Eagles","collectionName":"Hotel California","artworkUrl100":"http://img/100x100.jpg"}]}`))
	}))
	defer server.Close()

	client, err := NewITunes(server.URL, "test-agent", time.Second, nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.Lookup(context.Background(), Song, Input{TrackName: "Hotel California"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "http://img/600x600.jpg" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestDeezerSongTitleOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != `track:"Hotel California"` {
			t.Fatalf("expected title-only deezer query, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"title":"Hotel California","artist":{"name":"Eagles"},"album":{"title":"Hotel California","cover_xl":"http://img/xl.jpg"}}]}`))
	}))
	defer server.Close()

	client, err := NewDeezer(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.Lookup(context.Background(), Song, Input{TrackName: "Hotel California"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "http://img/xl.jpg" {
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

func TestLastfmAlbumAcceptsRealAlbumPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/music/Oasis/(What's+the+Story)+Morning+Glory?" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`
			<div class="album-overview-cover-art js-focus-controls-container">
				<a href="/music/Oasis/(What%27s+the+Story)+Morning+Glory%3F/+images/1b217359e775a8b6a7bc443abe5b08c2" class="cover-art">
					<img src="https://lastfm-img.freetls.fastly.net/i/u/500x500/1b217359e775a8b6a7bc443abe5b08c2.jpg" alt="(What&#39;s the Story) Morning Glory?" loading="lazy" />
				</a>
			</div>
			<img src="https://lastfm-img.freetls.fastly.net/i/u/avatar170s/c6f59c1e5e7240a4c0d427abd71f3dbb.jpg" alt="Avatar for Oasis">
		`))
	}))
	defer server.Close()

	client, _ := NewLastfm(server.URL, "test-agent", time.Second)
	result, err := client.Lookup(context.Background(), Album, Input{ArtistName: "Oasis", AlbumName: "(What's the Story) Morning Glory?"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if result.URL != "https://lastfm-img.freetls.fastly.net/i/u/500x500/1b217359e775a8b6a7bc443abe5b08c2.jpg" || result.Source != "lastfm" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestLastfmRejectsAlbumThatDoesNotExist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`
			<img src="https://lastfm-img.freetls.fastly.net/i/u/ar0/c6f59c1e5e7240a4c0d427abd71f3dbb.jpg" alt="Avatar for Eagles">
			<img src="https://lastfm-img.freetls.fastly.net/i/u/300x300/0a61bfffd183d6d227d5b1ab43e91a82.jpg" alt="Greatest Hits">
		`))
	}))
	defer server.Close()

	client, _ := NewLastfm(server.URL, "test-agent", time.Second)
	// Last.fm renders the artist avatar and unrelated covers with HTTP 200 for
	// albums that do not exist; the album-overview-cover-art container is
	// absent, so the provider must not report a cover.
	if _, err := client.Lookup(context.Background(), Album, Input{ArtistName: "Eagles", AlbumName: "Imagine"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a non-existent album, got %v", err)
	}
}

func TestDeezerSearchAlbumCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/album" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "Imagine" {
			t.Fatalf("expected unquoted query, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"title":"More Than We Ever Imagined","artist":{"name":"The Oh Hellos"},"cover_xl":"http://img/other.jpg"},
			{"title":"Imagine","artist":{"name":"John Lennon"},"cover_xl":"http://img/imagine.jpg"},
			{"title":"Evolve","artist":{"name":"Imagine Dragons"},"cover_big":"http://img/evolve.jpg"}
		]}`))
	}))
	defer server.Close()

	client, _ := NewDeezer(server.URL, "test-agent", time.Second)
	results, err := client.Search(context.Background(), Album, Input{AlbumName: "Imagine"}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 candidates, got %d: %+v", len(results), results)
	}
	if results[1].AlbumName != "Imagine" || results[1].ArtistName != "John Lennon" {
		t.Fatalf("unexpected candidate: %+v", results[1])
	}
}

func TestITunesSearchAlbumCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("term"); got != "Imagine" {
			t.Fatalf("unexpected term: %q", got)
		}
		if got := r.URL.Query().Get("entity"); got != "album" {
			t.Fatalf("unexpected entity: %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Fatalf("unexpected limit: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultCount":2,"results":[
			{"collectionName":"Imagine (Reggae Version) - Single","artistName":"NIFANA","artworkUrl100":"http://img/a/100x100bb.jpg"},
			{"collectionName":"Imagine","artistName":"John Lennon","artworkUrl100":"http://img/b/100x100bb.jpg"}
		]}`))
	}))
	defer server.Close()

	client, _ := NewITunes(server.URL, "test-agent", time.Second, nil)
	results, err := client.Search(context.Background(), Album, Input{AlbumName: "Imagine"}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %+v", len(results), results)
	}
	if results[0].AlbumName != "Imagine (Reggae Version) - Single" || results[0].URL != "http://img/a/600x600bb.jpg" {
		t.Fatalf("unexpected candidate: %+v", results[0])
	}
	if results[1].ArtistName != "John Lennon" {
		t.Fatalf("unexpected candidate: %+v", results[1])
	}
}

type searchStubProvider struct {
	name    string
	results []Result
	err     error
	calls   int
}

func (p *searchStubProvider) Name() string { return p.name }
func (p *searchStubProvider) Lookup(_ context.Context, _ Kind, _ Input) (*Result, error) {
	p.calls++
	return &Result{URL: "http://lookup/cover.jpg", Source: p.name}, nil
}
func (p *searchStubProvider) Search(_ context.Context, _ Kind, _ Input, _ int) ([]Result, error) {
	p.calls++
	return p.results, p.err
}

func TestResolverSearchPrefersSearchProvider(t *testing.T) {
	sp := &searchStubProvider{name: "sp", results: []Result{{URL: "http://one.jpg", Source: "sp"}, {URL: "http://two.jpg", Source: "sp"}}}
	r := NewResolver(sp)
	results, err := r.Search(context.Background(), Album, Input{AlbumName: "Imagine"}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(results), results)
	}
	if sp.calls != 1 {
		t.Fatalf("expected one Search call, got %d", sp.calls)
	}
}

func TestResolverSearchFallsBackToLookup(t *testing.T) {
	sp := &searchStubProvider{name: "sp", err: ErrNotFound}
	r := NewResolver(sp)
	results, err := r.Search(context.Background(), Artist, Input{ArtistName: "Radiohead"}, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0].URL != "http://lookup/cover.jpg" {
		t.Fatalf("unexpected results: %+v", results)
	}
	if sp.calls != 2 {
		t.Fatalf("expected Search + Lookup fallback calls, got %d", sp.calls)
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
