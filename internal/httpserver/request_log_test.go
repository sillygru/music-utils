package httpserver

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

func TestRequestLogRecordsEveryRequest(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	logPath := filepath.Join(t.TempDir(), "request_log.db")

	cfg := rateLimitTestConfig()
	cfg.RequestLogEnabled = true
	cfg.RequestLogDBPath = logPath
	cfg.RequestLogRetentionDays = 30
	server, requestLogs := NewWithLogger(cfg, metadataDB, lyricsDB, nil, slog.Default())
	if requestLogs == nil {
		t.Fatal("expected a request log writer when enabled")
	}

	// A cache hit, a miss, and a health probe: every request must be logged.
	if response := performRequest(t, server.Handler, "/api/lyrics/search?q=example&limit=5"); response.Code != http.StatusOK {
		t.Fatalf("search failed: %d", response.Code)
	}
	if response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Unknown&artist_name=Artist"); response.Code != http.StatusNotFound {
		t.Fatalf("miss failed: %d", response.Code)
	}
	if response := performRequest(t, server.Handler, "/healthz"); response.Code != http.StatusOK {
		t.Fatalf("healthz failed: %d", response.Code)
	}

	// Shut down, then flush the request log synchronously: Shutdown does not
	// wait for RegisterOnShutdown hooks.
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
	if count != 3 {
		t.Fatalf("expected 3 logged requests, got %d", count)
	}

	// The search hit must carry its params, outcome, status, and cache timing.
	var outcome, params string
	var status int
	var cacheMs, upstreamMs int64
	err := logged.QueryRowContext(context.Background(), `
		SELECT o.name, l.params, l.status, l.cache_ms, l.upstream_ms
		FROM request_log l
		JOIN endpoints e ON e.id = l.endpoint_id
		JOIN outcomes o ON o.id = l.outcome_id
		WHERE e.name = '/api/lyrics/search'`).Scan(&outcome, &params, &status, &cacheMs, &upstreamMs)
	if err != nil {
		t.Fatalf("query search log row: %v", err)
	}
	if outcome != "local_hit" || params != "q=example&limit=5" || status != http.StatusOK {
		t.Fatalf("unexpected search log row: outcome=%q params=%q status=%d", outcome, params, status)
	}
	if cacheMs < 1 || upstreamMs != 0 {
		t.Fatalf("unexpected timings for a local hit: cache_ms=%d upstream_ms=%d", cacheMs, upstreamMs)
	}
}

func TestRequestLogRecordsUpstreamTiming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trackName":"Remote Song","artistName":"Remote Artist","albumName":"Remote Album","duration":200,"instrumental":false,"plainLyrics":"remote lyrics","syncedLyrics":""}`))
	}))
	defer upstream.Close()

	logPath := filepath.Join(t.TempDir(), "request_log.db")
	cfg := fallbackConfig(upstream.URL + "/api")
	cfg.RequestLogEnabled = true
	cfg.RequestLogDBPath = logPath
	cfg.RequestLogRetentionDays = 30

	metadataDB, lyricsDB := testHTTPDatabases(t)
	server, requestLogs := NewWithLogger(cfg, metadataDB, lyricsDB, nil, slog.Default())
	response := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Remote+Song&artist_name=Remote+Artist")
	if response.Code != http.StatusOK {
		t.Fatalf("expected fallback 200, got %d", response.Code)
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
	var outcome string
	var upstreamMs int64
	err := logged.QueryRowContext(context.Background(), `
		SELECT o.name, l.upstream_ms
		FROM request_log l
		JOIN outcomes o ON o.id = l.outcome_id
		WHERE l.endpoint_id = (SELECT id FROM endpoints WHERE name = '/api/lyrics/get')`).Scan(&outcome, &upstreamMs)
	if err != nil {
		t.Fatalf("query upstream log row: %v", err)
	}
	if outcome != "lrclib_fallback_hit" {
		t.Fatalf("expected lrclib_fallback_hit outcome, got %q", outcome)
	}
	if upstreamMs < 20 {
		t.Fatalf("expected the slow upstream call to be timed, got upstream_ms=%d", upstreamMs)
	}
}

func TestRequestLogDisabledCreatesNoDatabase(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	cfg := rateLimitTestConfig() // RequestLogEnabled is false on a zero Config
	cfg.RequestLogDBPath = filepath.Join(t.TempDir(), "should-not-exist.db")
	server := NewWithConfig(cfg, metadataDB, lyricsDB)

	if response := performRequest(t, server.Handler, "/api/lyrics/search?q=example"); response.Code != http.StatusOK {
		t.Fatalf("search failed: %d", response.Code)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := os.Stat(cfg.RequestLogDBPath); !os.IsNotExist(err) {
		t.Fatalf("expected no request log database when disabled, stat err=%v", err)
	}
}

func TestSplitTimingHelpersAccumulate(t *testing.T) {
	state := &requestState{}
	ctx := context.WithValue(context.Background(), requestStateKey{}, state)
	request := httptest.NewRequest(http.MethodGet, "/api/lyrics/get", nil).WithContext(ctx)

	setCacheDuration(request, 3*time.Millisecond)
	setCacheDuration(request, 2*time.Millisecond)
	setCacheDuration(request, 500*time.Microsecond) // sub-ms rounds up to 1ms
	setUpstreamDuration(request, 40*time.Millisecond)
	if state.cacheMs != 6 || state.upstreamMs != 40 {
		t.Fatalf("unexpected accumulated timings: cache=%d upstream=%d", state.cacheMs, state.upstreamMs)
	}

	// Requests without request-logging state must be harmless no-ops.
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	setCacheDuration(plain, time.Second)
	setUpstreamDuration(plain, time.Second)
}

func openRequestLog(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := db.Open(path, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open request log: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}
