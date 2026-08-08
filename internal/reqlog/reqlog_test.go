package reqlog

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sillygru/music-utils/internal/db"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	read, err := db.Open(path, db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("reopen request log: %v", err)
	}
	t.Cleanup(func() { _ = read.Close() })
	return read
}

func TestOpenAppliesStorageOptimizations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.db")
	w, err := Open(path, 30*24*time.Hour, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = w.Close() }()

	read := openTestDB(t, path)
	var autoVacuum int
	if err := read.QueryRowContext(context.Background(), "PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil {
		t.Fatalf("read auto_vacuum: %v", err)
	}
	if autoVacuum != 2 {
		t.Fatalf("expected auto_vacuum=INCREMENTAL, got %d", autoVacuum)
	}
	var journalMode string
	if err := read.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("expected WAL journal mode, got %q", journalMode)
	}
	// The append table must carry no secondary indexes: repeated text lives in
	// dictionary tables and the log stays a dense page stream.
	var indexes int
	if err := read.QueryRowContext(context.Background(),
		"SELECT count(*) FROM sqlite_master WHERE type='index' AND tbl_name='request_log' AND name NOT LIKE 'sqlite_autoindex_%'",
	).Scan(&indexes); err != nil {
		t.Fatalf("count request_log indexes: %v", err)
	}
	if indexes != 0 {
		t.Fatalf("expected no secondary indexes on request_log, got %d", indexes)
	}
}

func TestWriteFlushAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.db")
	w, err := Open(path, 0, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Log(Record{TS: time.Now(), Method: "GET", Endpoint: "/api/lyrics/get", Status: 200, Outcome: "local_hit", CacheMs: 3, Params: "track_name=x&artist_name=y"})
	w.Log(Record{TS: time.Now(), Method: "GET", Endpoint: "/api/lyrics/get", Status: 404, Outcome: "miss", UpstreamMs: 120, Params: "track_name=a&artist_name=b"})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	read := openTestDB(t, path)
	var count int
	if err := read.QueryRowContext(context.Background(), "SELECT count(*) FROM request_log").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows, got %d", count)
	}

	var endpoint, outcome, params string
	var status int
	var cacheMs, upstreamMs int64
	err = read.QueryRowContext(context.Background(), `
		SELECT e.name, o.name, l.params, l.status, l.cache_ms, l.upstream_ms
		FROM request_log l
		JOIN endpoints e ON e.id = l.endpoint_id
		JOIN outcomes o ON o.id = l.outcome_id
		ORDER BY l.id LIMIT 1`).Scan(&endpoint, &outcome, &params, &status, &cacheMs, &upstreamMs)
	if err != nil {
		t.Fatalf("read first row: %v", err)
	}
	if endpoint != "/api/lyrics/get" || outcome != "local_hit" || status != 200 {
		t.Fatalf("unexpected first row: endpoint=%q outcome=%q status=%d", endpoint, outcome, status)
	}
	if params != "track_name=x&artist_name=y" {
		t.Fatalf("unexpected params %q", params)
	}
	if cacheMs != 3 || upstreamMs != 0 {
		t.Fatalf("unexpected timings: cache_ms=%d upstream_ms=%d", cacheMs, upstreamMs)
	}

	// Dictionary tables absorb the repeated values: 1 endpoint, 1 method,
	// 2 outcomes across the two rows.
	for table, want := range map[string]int{"endpoints": 1, "methods": 1, "outcomes": 2} {
		if err := read.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("expected %d %s rows, got %d", want, table, count)
		}
	}
}

func TestPruneRemovesExpiredRowsAndKeepsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.db")
	w, err := Open(path, time.Hour, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Log(Record{TS: time.Now().Add(-2 * time.Hour), Method: "GET", Endpoint: "/api/x", Status: 404, Outcome: "miss"})
	w.Log(Record{TS: time.Now(), Method: "GET", Endpoint: "/api/x", Status: 200, Outcome: "local_hit"})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Prune through a fresh writer against the same file.
	w2, err := Open(path, time.Hour, testLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	w2.prune(time.Now())
	if err := w2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	read := openTestDB(t, path)
	var count int
	if err := read.QueryRowContext(context.Background(), "SELECT count(*) FROM request_log").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 surviving row after prune, got %d", count)
	}
	var ts int64
	if err := read.QueryRowContext(context.Background(), "SELECT ts FROM request_log").Scan(&ts); err != nil {
		t.Fatalf("read surviving ts: %v", err)
	}
	if ts < time.Now().Add(-time.Minute).UnixMilli() {
		t.Fatalf("expected the fresh row to survive, got ts %d", ts)
	}
}

func TestLogDropsWhenQueueFullWithoutBlocking(t *testing.T) {
	// No writer goroutine is running, so nothing drains the tiny channel: Log
	// must never block and must count the overflow.
	w := &Writer{
		ch:        make(chan Record, 1),
		logger:    testLogger(),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		pruneDone: make(chan struct{}),
		methods:   make(map[string]int64),
		endpoints: make(map[string]int64),
		outcomes:  make(map[string]int64),
	}
	record := Record{Method: "GET", Endpoint: "/api/x", Status: 200, Outcome: "local_hit"}
	for i := 0; i < 5; i++ {
		w.Log(record)
	}
	if got := w.dropped.Load(); got != 4 {
		t.Fatalf("expected 4 dropped records, got %d", got)
	}
}

func TestTruncateParamsBoundsWithoutSplittingRunes(t *testing.T) {
	long := strings.Repeat("a", 2048)
	if got := TruncateParams(long); len(got) != maxParamsLen {
		t.Fatalf("expected truncation to maxParamsLen, got %d", len(got))
	}
	// 600 two-byte runes = 1200 bytes; cutting at 1024 must not split a rune.
	multibyte := strings.Repeat("é", 600)
	got := TruncateParams(multibyte)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a UTF-8 rune: %q", got)
	}
	if len(got) > maxParamsLen {
		t.Fatalf("truncation exceeded maxParamsLen: %d", len(got))
	}
	// Short values pass through untouched.
	if got := TruncateParams("q=hello"); got != "q=hello" {
		t.Fatalf("short value changed: %q", got)
	}
}
