package httpserver

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
)

// kindCoverStub is a cover provider stub that returns per-kind candidates so
// prefetch tests exercise the album and artist search paths separately.
type kindCoverStub struct {
	name       string
	albums     []cover.Result
	artists    []cover.Result
	albumCalls int
	artistCalls int
}

func (c *kindCoverStub) Name() string { return c.name }
func (c *kindCoverStub) Lookup(_ context.Context, _ cover.Kind, _ cover.Input) (*cover.Result, error) {
	return nil, cover.ErrNotFound
}
func (c *kindCoverStub) Search(_ context.Context, kind cover.Kind, _ cover.Input, limit int) ([]cover.Result, error) {
	var src []cover.Result
	switch kind {
	case cover.Album:
		c.albumCalls++
		src = c.albums
	case cover.Artist:
		c.artistCalls++
		src = c.artists
	default:
		return nil, cover.ErrNotFound
	}
	out := make([]cover.Result, 0, len(src))
	for _, result := range src {
		if len(out) >= limit {
			break
		}
		out = append(out, result)
	}
	return out, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testPrefetcher builds a prefetcher wired to in-memory databases and cleans it
// up at the end of the test.
func testPrefetcher(t *testing.T, cfg config.Config, metadataDB, lyricsDB, coverDB *sql.DB, coverRes *cover.Resolver, lrclibClient *lrclib.Client) *prefetcher {
	t.Helper()
	p := newPrefetcher(cfg, metadataDB, lyricsDB, coverDB, coverRes, lrclibClient, newLyricsMissCache(), discardLogger())
	t.Cleanup(func() {
		if p != nil {
			p.Stop()
		}
	})
	return p
}

func eventually(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func prefetchTestDatabases(t *testing.T) (metadataDB, lyricsDB, coverDB *sql.DB) {
	t.Helper()
	metadataDB, lyricsDB = testHTTPDatabases(t)
	coverDB = testCoverDB(t)
	return metadataDB, lyricsDB, coverDB
}

// TestPrefetchFetchesRelatedContent proves a queued song gets its lyrics,
// album cover, and artist cover fetched and cached.
func TestPrefetchFetchesRelatedContent(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/get" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trackName":"Example Song","artistName":"Example Artist","albumName":"Example Album","duration":200,"instrumental":false,"plainLyrics":"prefetched lyrics","syncedLyrics":""}`))
	}))
	defer upstream.Close()

	client, err := lrclib.New(upstream.URL, "music-utils-test", time.Second)
	if err != nil {
		t.Fatalf("new lrclib client: %v", err)
	}
	resolver := cover.NewResolver(&kindCoverStub{
		name:   "stub",
		albums: []cover.Result{{URL: "http://img/album.jpg", Source: "stub", ArtistName: "Example Artist", AlbumName: "Example Album"}},
		artists: []cover.Result{{URL: "http://img/artist.jpg", Source: "stub", ArtistName: "Example Artist"}},
	})
	metadataDB, lyricsDB, coverDB := prefetchTestDatabases(t)
	p := testPrefetcher(t, config.Config{PrefetchEnabled: true, PrefetchPerMin: 100, PrefetchConcurrency: 2, PrefetchQueueSize: 16, PrefetchLyrics: true, PrefetchAlbumCover: true, PrefetchArtistCover: true}, metadataDB, lyricsDB, coverDB, resolver, client)

	p.Enqueue("Example Song", "Example Artist", "Example Album", 0)

	eventually(t, 3*time.Second, func() bool {
		_, lyrics, err := db.FindTrackExact(context.Background(), metadataDB, lyricsDB, "Example Song", "Example Artist", "Example Album", 0)
		return err == nil && lyricsAvailable(lyrics)
	})
	_, lyrics, err := db.FindTrackExact(context.Background(), metadataDB, lyricsDB, "Example Song", "Example Artist", "Example Album", 0)
	if err != nil || lyrics.PlainLyrics != "prefetched lyrics" {
		t.Fatalf("expected prefetched lyrics, got %+v err %v", lyrics, err)
	}

	eventually(t, 3*time.Second, func() bool {
		cached, cacheErr := db.FindCoverArt(context.Background(), coverDB, db.CoverAlbum, "Example Artist", "Example Album")
		return cacheErr == nil && cached.CoverURL != ""
	})
	album, _ := db.FindCoverArt(context.Background(), coverDB, db.CoverAlbum, "Example Artist", "Example Album")
	if album.CoverURL != "http://img/album.jpg" || album.CoverSource != "stub" {
		t.Fatalf("unexpected album cover: %+v", album)
	}

	eventually(t, 3*time.Second, func() bool {
		cached, cacheErr := db.FindCoverArt(context.Background(), coverDB, db.CoverArtist, "Example Artist", "")
		return cacheErr == nil && cached.CoverURL != ""
	})
	artist, _ := db.FindCoverArt(context.Background(), coverDB, db.CoverArtist, "Example Artist", "")
	if artist.CoverURL != "http://img/artist.jpg" || artist.CoverSource != "stub" {
		t.Fatalf("unexpected artist cover: %+v", artist)
	}

	if calls.Load() != 1 {
		t.Fatalf("expected one LRCLIB request, got %d", calls.Load())
	}
}

// TestPrefetchSkipsCachedTargets proves already-cached targets trigger no
// upstream spend at all.
func TestPrefetchSkipsCachedTargets(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	client, err := lrclib.New(upstream.URL, "music-utils-test", time.Second)
	if err != nil {
		t.Fatalf("new lrclib client: %v", err)
	}
	stub := &kindCoverStub{
		name:   "stub",
		albums: []cover.Result{{URL: "http://img/album.jpg", Source: "stub", ArtistName: "Example Artist", AlbumName: "Example Album"}},
		artists: []cover.Result{{URL: "http://img/artist.jpg", Source: "stub", ArtistName: "Example Artist"}},
	}
	metadataDB, lyricsDB, coverDB := prefetchTestDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	_ = db.UpsertCoverArt(context.Background(), coverDB, db.CoverAlbum, "Example Artist", "Example Album", "http://img/album.jpg", "stub")
	_ = db.UpsertCoverArt(context.Background(), coverDB, db.CoverArtist, "Example Artist", "", "http://img/artist.jpg", "stub")

	p := testPrefetcher(t, config.Config{PrefetchEnabled: true, PrefetchPerMin: 100, PrefetchConcurrency: 2, PrefetchQueueSize: 16, PrefetchLyrics: true, PrefetchAlbumCover: true, PrefetchArtistCover: true}, metadataDB, lyricsDB, coverDB, cover.NewResolver(stub), client)

	p.Enqueue("Example Song", "Example Artist", "Example Album", 203.5)

	// Give the worker time to run the job; even if it has, every target is
	// served from cache so no upstream call may happen.
	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("expected no LRCLIB requests for cached lyrics, got %d", calls.Load())
	}
	if stub.albumCalls != 0 || stub.artistCalls != 0 {
		t.Fatalf("expected no cover provider calls for cached covers, got album=%d artist=%d", stub.albumCalls, stub.artistCalls)
	}
}

// TestPrefetchDeduplicatesInFlight proves a duplicate enqueue of the same song
// is dropped before it can spend any upstream budget.
func TestPrefetchDeduplicatesInFlight(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trackName":"Example Song","artistName":"Example Artist","albumName":"Example Album","duration":200,"instrumental":false,"plainLyrics":"lyrics","syncedLyrics":""}`))
	}))
	defer upstream.Close()

	client, err := lrclib.New(upstream.URL, "music-utils-test", time.Second)
	if err != nil {
		t.Fatalf("new lrclib client: %v", err)
	}
	resolver := cover.NewResolver(&kindCoverStub{
		name:   "stub",
		albums: []cover.Result{{URL: "http://img/album.jpg", Source: "stub", ArtistName: "Example Artist", AlbumName: "Example Album"}},
		artists: []cover.Result{{URL: "http://img/artist.jpg", Source: "stub", ArtistName: "Example Artist"}},
	})
	metadataDB, lyricsDB, coverDB := prefetchTestDatabases(t)
	p := testPrefetcher(t, config.Config{PrefetchEnabled: true, PrefetchPerMin: 100, PrefetchConcurrency: 2, PrefetchQueueSize: 16, PrefetchLyrics: true, PrefetchAlbumCover: true, PrefetchArtistCover: true}, metadataDB, lyricsDB, coverDB, resolver, client)

	p.Enqueue("Example Song", "Example Artist", "Example Album", 0)
	p.Enqueue("Example Song", "Example Artist", "Example Album", 0)

	eventually(t, 3*time.Second, func() bool { return calls.Load() >= 1 })
	time.Sleep(150 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("expected deduplicated single LRCLIB request, got %d", calls.Load())
	}
}

// TestPrefetchRespectsBudget proves background spend is capped at the
// configured per-minute budget: once the budget is exhausted, later targets
// and jobs are skipped without calling any provider.
func TestPrefetchRespectsBudget(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trackName":"First Song","artistName":"Example Artist","albumName":"First Album","duration":200,"instrumental":false,"plainLyrics":"lyrics","syncedLyrics":""}`))
	}))
	defer upstream.Close()

	client, err := lrclib.New(upstream.URL, "music-utils-test", time.Second)
	if err != nil {
		t.Fatalf("new lrclib client: %v", err)
	}
	stub := &kindCoverStub{
		name:   "stub",
		albums: []cover.Result{{URL: "http://img/album.jpg", Source: "stub", ArtistName: "Example Artist", AlbumName: "First Album"}},
		artists: []cover.Result{{URL: "http://img/artist.jpg", Source: "stub", ArtistName: "Example Artist"}},
	}
	metadataDB, lyricsDB, coverDB := prefetchTestDatabases(t)
	p := testPrefetcher(t, config.Config{PrefetchEnabled: true, PrefetchPerMin: 1, PrefetchConcurrency: 1, PrefetchQueueSize: 16, PrefetchLyrics: true, PrefetchAlbumCover: true, PrefetchArtistCover: true}, metadataDB, lyricsDB, coverDB, cover.NewResolver(stub), client)

	p.Enqueue("First Song", "Example Artist", "First Album", 0)
	p.Enqueue("Second Song", "Example Artist", "Second Album", 0)

	eventually(t, 3*time.Second, func() bool {
		_, lyrics, err := db.FindTrackExact(context.Background(), metadataDB, lyricsDB, "First Song", "Example Artist", "First Album", 0)
		return err == nil && lyricsAvailable(lyrics)
	})
	// Give the sequential worker time to run the second job; nothing may
	// consume budget beyond the single allowed call.
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one upstream call under the budget, got %d", calls.Load())
	}
	if stub.albumCalls != 0 || stub.artistCalls != 0 {
		t.Fatalf("expected cover providers skipped once budget was exhausted, got album=%d artist=%d", stub.albumCalls, stub.artistCalls)
	}
}

// TestPrefetchDisabledNoOp proves Enqueue and Stop are safe on a nil prefetcher
// and that a disabled config constructs none.
func TestPrefetchDisabledNoOp(t *testing.T) {
	var p *prefetcher
	p.Enqueue("Example Song", "Example Artist", "Example Album", 0)
	p.Stop()

	metadataDB, lyricsDB, coverDB := prefetchTestDatabases(t)
	if got := newPrefetcher(config.Config{PrefetchEnabled: false}, metadataDB, lyricsDB, coverDB, nil, nil, newLyricsMissCache(), discardLogger()); got != nil {
		t.Fatalf("expected nil prefetcher when disabled, got %+v", got)
	}
}
