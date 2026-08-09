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
	w, err := Open(path, &Options{Retention: 30 * 24 * time.Hour}, testLogger())
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
	w, err := Open(path, &Options{Retention: 0}, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Log(Record{TS: time.Now(), Method: "GET", Endpoint: "/api/lyrics/get", Status: 200, Outcome: "local_hit", CacheMs: 3, Params: "track_name=x&artist_name=y", UserAgent: "test-agent/1.0"})
	w.Log(Record{TS: time.Now(), Method: "GET", Endpoint: "/api/lyrics/get", Status: 404, Outcome: "miss", UpstreamMs: 120, Params: "track_name=a&artist_name=b", UserAgent: "test-agent/1.0"})
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

	var endpoint, outcome, params, userAgent string
	var status int
	var cacheMs, upstreamMs int64
	err = read.QueryRowContext(context.Background(), `
		SELECT e.name, o.name, l.params, l.status, l.cache_ms, l.upstream_ms, l.user_agent
		FROM request_log l
		JOIN endpoints e ON e.id = l.endpoint_id
		JOIN outcomes o ON o.id = l.outcome_id
		ORDER BY l.id LIMIT 1`).Scan(&endpoint, &outcome, &params, &status, &cacheMs, &upstreamMs, &userAgent)
	if err != nil {
		t.Fatalf("read first row: %v", err)
	}
	if endpoint != "/api/lyrics/get" || outcome != "local_hit" || status != 200 {
		t.Fatalf("unexpected first row: endpoint=%q outcome=%q status=%d", endpoint, outcome, status)
	}
	if params != "track_name=x&artist_name=y" {
		t.Fatalf("unexpected params %q", params)
	}
	if userAgent != "test-agent/1.0" {
		t.Fatalf("unexpected user_agent %q", userAgent)
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
	w, err := Open(path, &Options{Retention: time.Hour}, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Log(Record{TS: time.Now().Add(-2 * time.Hour), Method: "GET", Endpoint: "/api/x", Status: 404, Outcome: "miss"})
	w.Log(Record{TS: time.Now(), Method: "GET", Endpoint: "/api/x", Status: 200, Outcome: "local_hit"})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Prune through a fresh writer against the same file.
	w2, err := Open(path, &Options{Retention: time.Hour}, testLogger())
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

func TestTruncateUserAgentBoundsWithoutTruncating(t *testing.T) {
	long := strings.Repeat("a", 512)
	if got := TruncateUserAgent(long); len(got) != maxUserAgentLen {
		t.Fatalf("expected truncation to maxUserAgentLen, got %d", len(got))
	}
	// 200 two-byte runes = 400 bytes; cutting at 256 must not split a rune.
	multibyte := strings.Repeat("é", 200)
	got := TruncateUserAgent(multibyte)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a UTF-8 rune: %q", got)
	}
	if len(got) > maxUserAgentLen {
		t.Fatalf("truncation exceeded maxUserAgentLen: %d", len(got))
	}
	// Short values pass through untouched, including exactly the cap.
	if got := TruncateUserAgent("my-agent/1.0"); got != "my-agent/1.0" {
		t.Fatalf("short value changed: %q", got)
	}
	if got := TruncateUserAgent(strings.Repeat("b", maxUserAgentLen)); len(got) != maxUserAgentLen {
		t.Fatalf("cap-length value changed: %d", len(got))
	}
}

func TestNormalizeUserAgentCollapsesKnownClients(t *testing.T) {
	w := &Writer{uaOptimize: true, uaSaveUnknown: true}
	cases := map[string]string{
		"curl/8.5.0 (x86_64-pc-linux-gnu) libcurl/8.5.0 OpenSSL/3.2.1": "curl",
		"Wget/1.21.4":            "wget",
		"Go-http-client/1.1":     "go-http-client",
		"python-requests/2.31.0": "python-requests",
		"aiohttp/3.9.1":          "python-aiohttp",
		"Mozilla/5.0 ... Chrome/120.0 Safari/537.36":   "browser-chromium",
		"Mozilla/5.0 ... Firefox/122.0":                "browser-firefox",
		"Mozilla/5.0 ... Edg/120.0":                    "browser-edge",
		"Mozilla/5.0 ... Version/17.2 Safari/605.1.15": "browser-webkit",
		"PostmanRuntime/7.36.0":                        "postman",
		"test-agent/1.0 custom":                        "test-agent/1.0 custom",
	}
	for ua, want := range cases {
		if got := w.normalizeUserAgent(ua); got != want {
			t.Errorf("normalizeUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestNormalizeUserAgentUnknownPolicy(t *testing.T) {
	// When optimize is off, the full string is always kept.
	off := &Writer{uaOptimize: false, uaSaveUnknown: false}
	if got := off.normalizeUserAgent("some/unknown client"); got != "some/unknown client" {
		t.Fatalf("expected full UA with optimize off, got %q", got)
	}
	// Optimize on + saveUnknown off drops unknown UAs.
	drop := &Writer{uaOptimize: true, uaSaveUnknown: false}
	if got := drop.normalizeUserAgent("some/unknown client"); got != "" {
		t.Fatalf("expected unknown UA dropped, got %q", got)
	}
	// Known UAs are still collapsed even when saving unknown is off.
	if got := drop.normalizeUserAgent("curl/8.0.0"); got != "curl" {
		t.Fatalf("expected known curl to still collapse, got %q", got)
	}
	// Empty UA is always empty.
	if got := drop.normalizeUserAgent(""); got != "" {
		t.Fatalf("expected empty UA to stay empty, got %q", got)
	}
}

func TestNormalizeUserAgentOverflowBounded(t *testing.T) {
	w := &Writer{uaOptimize: true, uaSaveUnknown: true}
	long := "curl/" + strings.Repeat("a", 4096)
	if got := w.normalizeUserAgent(long); got != "curl" {
		t.Fatalf("expected known curl to collapse regardless of length, got %q", got)
	}
	// An unknown long UA is bounded by TruncateUserAgent.
	unknown := &Writer{uaOptimize: true, uaSaveUnknown: true}
	got := unknown.normalizeUserAgent(strings.Repeat("Ü", 512))
	if len(got) > maxUserAgentLen || len(got) == 0 {
		t.Fatalf("expected unknown long UA bounded to maxUserAgentLen, got len %d", len(got))
	}
}

func TestNormalizeUserAgentSaveUnknownDisabledDropsLongUnknown(t *testing.T) {
	w := &Writer{uaOptimize: true, uaSaveUnknown: false}
	if got := w.normalizeUserAgent(strings.Repeat("xyz", 512)); got != "" {
		t.Fatalf("expected long unknown UA dropped, got len %d", len(got))
	}
}

func TestOpenMigratesExistingDatabaseWithUserAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.db")
	// Build a pre-migration database: create the schema minus user_agent and
	// insert a row, mirroring what an older build left behind.
	legacy := openTestDB(t, path)
	if _, err := legacy.Exec(`
		CREATE TABLE request_log (
			id INTEGER PRIMARY KEY,
			ts INTEGER NOT NULL,
			method_id INTEGER NOT NULL,
			endpoint_id INTEGER NOT NULL,
			status INTEGER NOT NULL,
			outcome_id INTEGER NOT NULL,
			cache_ms INTEGER NOT NULL DEFAULT 0,
			upstream_ms INTEGER NOT NULL DEFAULT 0,
			params TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := legacy.Exec(`
		INSERT INTO request_log (ts, method_id, endpoint_id, status, outcome_id) VALUES (0, 1, 1, 200, 1)`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// Open via the normal path must add the column and preserve rows.
	w, err := Open(path, &Options{Retention: 0}, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = w.Close() }()

	read := openTestDB(t, path)
	var hasCol int
	if err := read.QueryRowContext(context.Background(),
		"SELECT count(*) FROM pragma_table_info('request_log') WHERE name='user_agent'").Scan(&hasCol); err != nil {
		t.Fatalf("check user_agent column: %v", err)
	}
	if hasCol != 1 {
		t.Fatalf("expected user_agent column after migration, found %d", hasCol)
	}
	var count int
	if err := read.QueryRowContext(context.Background(), "SELECT count(*) FROM request_log").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the legacy row to survive migration, got %d rows", count)
	}
}

func TestRequestsTodayCountsLoggedRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.db")
	w, err := Open(path, &Options{Retention: 0}, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.RequestsToday(); got != 0 {
		t.Fatalf("expected 0 before any requests, got %d", got)
	}
	rec := Record{TS: time.Now(), Method: "GET", Endpoint: "/api/lyrics/get", Status: 200, Outcome: "local_hit"}
	w.Log(rec)
	w.Log(rec)
	w.Log(rec)
	if got := w.RequestsToday(); got != 3 {
		t.Fatalf("expected 3 counted requests, got %d", got)
	}
}

func TestRequestsTodayExcludesConfiguredPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.db")
	w, err := Open(path, &Options{Retention: 0, ExcludeCountPath: "/api/stats/requests-today"}, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = w.Close() }()

	now := time.Now()
	w.Log(Record{TS: now, Endpoint: "/api/stats/requests-today"})
	w.Log(Record{TS: now, Endpoint: "/api/stats/requests-today"})
	w.Log(Record{TS: now, Endpoint: "/api/lyrics/get"})
	w.Log(Record{TS: now, Endpoint: "/api/lyrics/get"})
	if got := w.RequestsToday(); got != 2 {
		t.Fatalf("expected only non-stats requests counted, got %d", got)
	}
}

func TestRequestsTodaySeedsFromPersistedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.db")
	w, err := Open(path, &Options{Retention: 0}, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.Log(Record{TS: time.Now(), Endpoint: "/api/lyrics/get"})
	w.Log(Record{TS: time.Now(), Endpoint: "/api/lyrics/get"})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A fresh process must recover today's count from the stored rows.
	w2, err := Open(path, &Options{Retention: 0}, testLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = w2.Close() }()
	if got := w2.RequestsToday(); got != 2 {
		t.Fatalf("expected 2 recovered requests today, got %d", got)
	}
	w2.Log(Record{TS: time.Now(), Endpoint: "/api/lyrics/get"})
	if got := w2.RequestsToday(); got != 3 {
		t.Fatalf("expected 3 after a live request, got %d", got)
	}
}

func TestRequestsTodayResetsOnNewDay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request_log.db")
	w, err := Open(path, &Options{Retention: 0}, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = w.Close() }()

	yesterday := time.Now().AddDate(0, 0, -1)
	w.Log(Record{TS: yesterday, Endpoint: "/api/lyrics/get"})
	// The counter holds yesterday's day; reading it for today resets to zero.
	if got := w.RequestsToday(); got != 0 {
		t.Fatalf("expected stale-day counter to read 0, got %d", got)
	}
	w.Log(Record{TS: time.Now(), Endpoint: "/api/lyrics/get"})
	if got := w.RequestsToday(); got != 1 {
		t.Fatalf("expected 1 for today's request, got %d", got)
	}
}
