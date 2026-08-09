package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

func testCoverDatabase(t *testing.T) *sql.DB {
	t.Helper()
	coverDB, err := db.Open(":memory:", db.Config{
		MmapSize:     512 * 1024 * 1024,
		CacheSizeKB:  -64000,
		MaxOpenConns: 1,
	})
	if err != nil {
		t.Fatalf("open cover test database: %v", err)
	}
	t.Cleanup(func() { _ = coverDB.Close() })
	if err := db.MigrateCover(context.Background(), coverDB); err != nil {
		t.Fatalf("migrate cover test database: %v", err)
	}
	return coverDB
}

// seedStatsData fills the caches with a known mix: two tracks sharing one
// (case-normalized) name, both with lyrics and a song cover URL; a third
// metadata-only track; plus one positive album cover, one positive artist
// cover, and one negative album cover entry that must not count.
func seedStatsData(t *testing.T, metadataDB, lyricsDB, coverDB *sql.DB) {
	t.Helper()
	ctx := context.Background()
	tracks := []db.Track{
		{Name: "Paranoid Android", ArtistName: "Radiohead", AlbumName: "OK Computer", Duration: 383, CoverURL: "http://cover/song/1"},
		{Name: "Paranoid Android", ArtistName: "A Cover Band", AlbumName: "Tribute", Duration: 300, CoverURL: "http://cover/song/2"},
		{Name: "No Surprises", ArtistName: "Radiohead", AlbumName: "OK Computer", Duration: 229},
	}
	for i, track := range tracks {
		var err error
		if i < 2 {
			// Shared lyrics content deduplicates to one lyrics row across two
			// song-to-lyrics associations.
			_, _, err = db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, db.Lyrics{PlainLyrics: "shared lyrics"})
		} else {
			_, err = db.UpsertTrackMetadata(ctx, metadataDB, track)
		}
		if err != nil {
			t.Fatalf("seed track %d: %v", i, err)
		}
	}
	if err := db.UpsertCoverArt(ctx, coverDB, db.CoverAlbum, "Radiohead", "OK Computer", "http://cover/album", "deezer"); err != nil {
		t.Fatalf("seed album cover: %v", err)
	}
	if err := db.UpsertCoverArt(ctx, coverDB, db.CoverArtist, "Radiohead", "", "http://cover/artist", "deezer"); err != nil {
		t.Fatalf("seed artist cover: %v", err)
	}
	if err := db.UpsertCoverArt(ctx, coverDB, db.CoverAlbum, "Radiohead", "Missing Album", "", ""); err != nil {
		t.Fatalf("seed negative album cover: %v", err)
	}
}

// serveHandler invokes a stats handler directly against an in-memory request,
// bypassing the full server middleware so the handler tests never touch rate
// limits, the request log, or upstream providers.
func serveHandler(t *testing.T, handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestStatsMetadataHandlerCountsCachedSongs(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	coverDB := testCoverDatabase(t)
	seedStatsData(t, metadataDB, lyricsDB, coverDB)

	response := serveHandler(t, statsMetadataHandler(metadataDB), statsMetadataPath)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		MetadataSongs int64 `json:"metadataSongs"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode metadata response: %v", err)
	}
	if body.MetadataSongs != 3 {
		t.Fatalf("expected 3 cached metadata songs, got %d", body.MetadataSongs)
	}
}

func TestStatsLyricsHandlerCountsCachedLyrics(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	coverDB := testCoverDatabase(t)
	seedStatsData(t, metadataDB, lyricsDB, coverDB)

	response := serveHandler(t, statsLyricsHandler(lyricsDB), statsLyricsPath)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		LyricsSongs int64 `json:"lyricsSongs"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode lyrics response: %v", err)
	}
	// Two tracks share one deduplicated lyrics row, but each has its own
	// association, so the count is songs-with-lyrics, not lyrics rows.
	if body.LyricsSongs != 2 {
		t.Fatalf("expected 2 songs with cached lyrics, got %d", body.LyricsSongs)
	}
}

func TestStatsSongsHandlerCountsDistinctTrackNames(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	coverDB := testCoverDatabase(t)
	seedStatsData(t, metadataDB, lyricsDB, coverDB)

	response := serveHandler(t, statsSongsHandler(metadataDB), statsSongsPath)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Songs int64 `json:"songs"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode songs response: %v", err)
	}
	// "Paranoid Android" and "paranoid android" normalize to one distinct name.
	if body.Songs != 2 {
		t.Fatalf("expected 2 distinct individual track names, got %d", body.Songs)
	}
}

func TestStatsCoversHandlerCountsAllCoverKinds(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	coverDB := testCoverDatabase(t)
	seedStatsData(t, metadataDB, lyricsDB, coverDB)

	response := serveHandler(t, statsCoversHandler(metadataDB, coverDB), statsCoversPath)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Covers       int64 `json:"covers"`
		SongCovers   int64 `json:"songCovers"`
		AlbumCovers  int64 `json:"albumCovers"`
		ArtistCovers int64 `json:"artistCovers"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode covers response: %v", err)
	}
	if body.Covers != 4 || body.SongCovers != 2 || body.AlbumCovers != 1 || body.ArtistCovers != 1 {
		t.Fatalf("unexpected covers counts: %+v", body)
	}
}

func TestStatsTotalHandlerSumsEverything(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	coverDB := testCoverDatabase(t)
	seedStatsData(t, metadataDB, lyricsDB, coverDB)

	response := serveHandler(t, statsTotalHandler(metadataDB, lyricsDB, coverDB), statsTotalPath)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Total    int64 `json:"total"`
		Metadata int64 `json:"metadata"`
		Lyrics   int64 `json:"lyrics"`
		Covers   int64 `json:"covers"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode total response: %v", err)
	}
	// Everything summed: 3 metadata + 2 lyrics + 4 covers.
	if body.Total != 9 || body.Metadata != 3 || body.Lyrics != 2 || body.Covers != 4 {
		t.Fatalf("unexpected total breakdown: %+v", body)
	}
}

func TestStatsHandlersFailClosedWithoutDatabase(t *testing.T) {
	for _, handler := range []http.HandlerFunc{
		statsMetadataHandler(nil),
		statsLyricsHandler(nil),
		statsSongsHandler(nil),
		statsCoversHandler(nil, nil),
		statsTotalHandler(nil, nil, nil),
	} {
		response := serveHandler(t, handler, "/api/stats/test")
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 when the cache database is unavailable, got %d", response.Code)
		}
	}
}

func TestStatsEndpointsDisabledAreNotFound(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	cfg := rateLimitTestConfig() // StatsEndpoints is empty: all endpoints opt-out
	cfg.RateLimitPerSec = 1000
	cfg.RateLimitPerMin = 100000
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	for _, path := range []string{statsMetadataPath, statsLyricsPath, statsCoversPath, statsTotalPath, statsSongsPath} {
		if response := performRequest(t, server.Handler, path); response.Code != http.StatusNotFound {
			t.Fatalf("expected %s to 404 when disabled, got %d", path, response.Code)
		}
	}
}

func TestStatsEndpointsOnlySelectedServed(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	cfg := rateLimitTestConfig()
	cfg.RateLimitPerSec = 1000
	cfg.RateLimitPerMin = 100000
	cfg.StatsEndpoints = []string{"metadata"}
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	if response := performRequest(t, server.Handler, statsMetadataPath); response.Code != http.StatusOK {
		t.Fatalf("expected the selected metadata endpoint to serve 200, got %d", response.Code)
	}
	for _, path := range []string{statsLyricsPath, statsCoversPath, statsTotalPath, statsSongsPath} {
		if response := performRequest(t, server.Handler, path); response.Code != http.StatusNotFound {
			t.Fatalf("expected unselected %s to 404, got %d", path, response.Code)
		}
	}
}

func TestStatsRequestsNotWrittenToRequestLog(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	logPath := filepath.Join(t.TempDir(), "request_log.db")

	cfg := rateLimitTestConfig()
	cfg.RateLimitPerSec = 1000
	cfg.RateLimitPerMin = 100000
	cfg.RequestLogEnabled = true
	cfg.RequestLogDBPath = logPath
	cfg.RequestLogRetentionDays = 30
	cfg.StatsEndpoints = []string{"metadata"}
	server, requestLogs := NewWithLogger(cfg, metadataDB, lyricsDB, nil, slog.Default())
	if requestLogs == nil {
		t.Fatal("expected a request log writer when enabled")
	}

	// One regular API request is logged; stats polls must never be.
	if response := performRequest(t, server.Handler, "/api/lyrics/search?q=example&limit=5"); response.Code != http.StatusOK {
		t.Fatalf("search failed: %d: %s", response.Code, response.Body.String())
	}
	for i := 0; i < 2; i++ {
		if response := performRequest(t, server.Handler, statsMetadataPath); response.Code != http.StatusOK {
			t.Fatalf("stats poll failed: %d: %s", response.Code, response.Body.String())
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := requestLogs.Close(); err != nil {
		t.Fatalf("close request log: %v", err)
	}

	logged := openRequestLog(t, logPath)
	var count int
	if err := logged.QueryRowContext(context.Background(), "SELECT count(*) FROM request_log").Scan(&count); err != nil {
		t.Fatalf("count request log: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected only the regular request logged (stats polls excluded), got %d rows", count)
	}
	for _, path := range []string{statsMetadataPath, statsLyricsPath, statsCoversPath, statsTotalPath, statsSongsPath} {
		var rows int
		err := logged.QueryRowContext(context.Background(),
			"SELECT count(*) FROM request_log WHERE endpoint_id = (SELECT id FROM endpoints WHERE name = ?)", path).Scan(&rows)
		if err != nil {
			t.Fatalf("count %s log rows: %v", path, err)
		}
		if rows != 0 {
			t.Fatalf("expected no log rows for %s, got %d", path, rows)
		}
	}
}
