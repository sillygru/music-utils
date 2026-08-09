// Package reqlog records a durable access log for every HTTP request in a
// dedicated SQLite database that is optimized for small size and fast append
// writes. It is operational data only: it is never served by the API and is
// never included in `music-utils export` seed dumps.
package reqlog

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

//go:embed reqlog_schema.sql
var schemaFS embed.FS

const (
	// maxParamsLen caps how many bytes of the raw query string are kept per
	// row; client-supplied values beyond this are dropped.
	maxParamsLen = 1024
	// maxUserAgentLen caps how many bytes of the client User-Agent header are
	// kept per row; User-Agent strings are unbounded client input.
	maxUserAgentLen = 256
	// channelCapacity bounds buffered records so a slow disk can never make
	// request handling wait; overflow is counted and dropped.
	channelCapacity = 4096
	// batchSize and flushEvery coalesce queued records into one transaction
	// each, trading a little latency for far fewer write round trips.
	batchSize  = 256
	flushEvery = 250 * time.Millisecond
	// pruneEvery is how often expired rows are deleted and freed pages are
	// reclaimed so the file stays dense.
	pruneEvery = 24 * time.Hour
	// vacuumChunk bounds how many free pages each prune pass reclaims.
	vacuumChunk = 2048
	// writeTimeout bounds a single flush or prune pass.
	writeTimeout = 5 * time.Second
)

// Options tunes request log persistence and storage costs.
type Options struct {
	// Retention is how long rows are kept; zero or less keeps rows forever.
	Retention time.Duration
	// UAOptimize collapses well-known User-Agent strings to short canonical
	// tokens (e.g. "curl/8.5.0 ..." -> "curl") to shrink the request_log file.
	// A zero value means the default (true) is used.
	UAOptimize *bool
	// UASaveUnknown controls what happens when a User-Agent is not recognized.
	// When true (default) the full string is stored; when false it is dropped
	// as empty to minimize storage. A nil pointer means the default (true)
	// applies; the flag only matters when UAOptimize is enabled.
	UASaveUnknown *bool
}

// retention returns the configured retention window, defaulting to "kept
// forever" (zero duration) when opts is nil.
func retention(opts *Options) time.Duration {
	if opts == nil {
		return 0
	}
	return opts.Retention
}

// Record describes one HTTP request.
type Record struct {
	TS         time.Time
	Method     string
	Endpoint   string // URL path, e.g. /api/lyrics/get
	Status     int
	Outcome    string // local_hit, provider_fallback_hit, miss, rate_limited, ...
	CacheMs    int64  // time spent in the local database/cache lookup
	UpstreamMs int64  // time spent talking to upstream providers
	Params     string // raw query string
	UserAgent  string // client User-Agent header
}

// Writer is a background, batched writer for the request log database. Log
// never blocks the caller; records are queued and flushed in batches. Close
// flushes anything queued and shuts the maintenance loop down.
type Writer struct {
	db            *sql.DB
	ch            chan Record
	logger        *slog.Logger
	retention     time.Duration
	uaOptimize    bool
	uaSaveUnknown bool

	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
	pruneDone chan struct{}

	// Dictionary id caches; only touched by the single writer goroutine.
	methods   map[string]int64
	endpoints map[string]int64
	outcomes  map[string]int64

	dropped       atomic.Int64
	writeFailures atomic.Int64
}

// Open opens (creating if needed) the request log database at path and starts
// its background writer. opts may be nil for package defaults (keep rows
// forever, UA optimization on, unknown UAs saved).
func Open(path string, opts *Options, logger *slog.Logger) (*Writer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	uaOptimize := true
	uaSaveUnknown := true
	if opts != nil {
		if opts.UAOptimize != nil {
			uaOptimize = *opts.UAOptimize
		}
		if opts.UASaveUnknown != nil {
			uaSaveUnknown = *opts.UASaveUnknown
		}
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("request log database path is empty")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create request log directory: %w", err)
		}
	}
	database, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open request log database: %w", err)
	}
	// The writer goroutine and the maintenance goroutine each use a pooled
	// connection; WAL keeps their writes serialized with a short busy timeout.
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(2)

	if err := database.PingContext(context.Background()); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("ping request log database: %w", err)
	}
	schema, err := schemaFS.ReadFile("reqlog_schema.sql")
	if err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("read request log schema: %w", err)
	}
	if _, err := database.Exec(string(schema)); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("apply request log schema: %w", err)
	}
	// Older request log databases predate the user_agent column. CREATE TABLE
	// IF NOT EXISTS is a no-op for them, so add the column in place rather than
	// forcing a rebuild; this is idempotent and safe to run on every open.
	if _, err := database.Exec(
		"ALTER TABLE request_log ADD COLUMN user_agent TEXT NOT NULL DEFAULT ''",
	); err != nil {
		// SQLite returns "duplicate column name" when the column already
		// exists, which just means this database is already current.
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			_ = database.Close()
			return nil, fmt.Errorf("migrate request log schema: %w", err)
		}
	}
	w := &Writer{
		db:            database,
		ch:            make(chan Record, channelCapacity),
		logger:        logger,
		retention:     retention(opts),
		uaOptimize:    uaOptimize,
		uaSaveUnknown: uaSaveUnknown,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		pruneDone:     make(chan struct{}),
		methods:       make(map[string]int64),
		endpoints:     make(map[string]int64),
		outcomes:      make(map[string]int64),
	}
	go w.run()
	go w.pruneLoop()
	return w, nil
}

// dsn builds a SQLite URI that favors small files and fast append writes:
// WAL with normal sync for throughput, an in-memory temp store, a modest page
// cache, and incremental auto-vacuum so pruned space is reclaimable.
func dsn(path string) string {
	base := "file:" + url.PathEscape(path)
	if path == ":memory:" {
		base = "file::memory:"
	}
	return base + "?" + strings.Join([]string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=temp_store(MEMORY)",
		"_pragma=cache_size(-8192)",
		"_pragma=foreign_keys(ON)",
		"_pragma=busy_timeout(5000)",
		"_pragma=auto_vacuum(INCREMENTAL)",
	}, "&")
}

// Log queues one request record. It never blocks: when the queue is full the
// record is dropped and counted, so request-handling latency never depends on
// log persistence.
func (w *Writer) Log(rec Record) {
	select {
	case w.ch <- rec:
	default:
		dropped := w.dropped.Add(1)
		if dropped == 1 || dropped%1024 == 0 {
			w.logger.Warn("request log queue full, dropping records", "dropped", dropped)
		}
	}
}

func (w *Writer) run() {
	defer close(w.done)
	batch := make([]Record, 0, batchSize)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	for {
		select {
		case rec, ok := <-w.ch:
			if !ok {
				// Defensive: the channel is never closed by the writer, but if
				// it ever were, flush what is queued rather than dropping it.
				w.flush(batch)
				return
			}
			batch = append(batch, rec)
			if len(batch) >= batchSize {
				w.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(batch)
				batch = batch[:0]
			}
		case <-w.stop:
			// Close (and hence shutdown) may race buffered records, so drain
			// the channel before the final flush instead of letting the select
			// pick stop over queued records and lose them.
			for {
				select {
				case rec := <-w.ch:
					batch = append(batch, rec)
					if len(batch) >= batchSize {
						w.flush(batch)
						batch = batch[:0]
					}
				default:
					w.flush(batch)
					return
				}
			}
		}
	}
}

// flush commits a batch of records in a single transaction, resolving each
// method/endpoint/outcome through the shared dictionary tables.
func (w *Writer) flush(batch []Record) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		w.writeFailure(err)
		return
	}
	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO request_log (ts, method_id, endpoint_id, status, outcome_id, cache_ms, upstream_ms, params, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		w.writeFailure(err)
		return
	}
	for i := range batch {
		rec := &batch[i]
		methodID, err := w.dictionaryID(ctx, tx, "methods", rec.Method, w.methods)
		if err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			w.writeFailure(err)
			return
		}
		endpointID, err := w.dictionaryID(ctx, tx, "endpoints", rec.Endpoint, w.endpoints)
		if err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			w.writeFailure(err)
			return
		}
		outcomeID, err := w.dictionaryID(ctx, tx, "outcomes", rec.Outcome, w.outcomes)
		if err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			w.writeFailure(err)
			return
		}
		if _, err := statement.ExecContext(ctx,
			rec.TS.UnixMilli(), methodID, endpointID, rec.Status, outcomeID,
			rec.CacheMs, rec.UpstreamMs, TruncateParams(rec.Params),
			w.normalizeUserAgent(rec.UserAgent),
		); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			w.writeFailure(err)
			return
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		w.writeFailure(err)
		return
	}
	if err := tx.Commit(); err != nil {
		w.writeFailure(err)
	}
}

// canonicalUAs maps a short, case-insensitive substring to the compact token
// stored in the request log. Substrings are matched against a lowercase copy
// of the User-Agent header; tokens are stored verbatim. Order matters: more
// specific or more common markers must appear before broader ones.
var canonicalUAs = []struct {
	needle string
	token  string
}{
	{"curl", "curl"},
	{"wget", "wget"},
	{"go-http-client", "go-http-client"},
	{"python-requests", "python-requests"},
	{"python-urllib", "python-urllib"},
	{"aiohttp", "python-aiohttp"},
	{"httpx", "python-httpx"},
	{"axios", "axios"},
	{"node-fetch", "node-fetch"},
	{"node", "node"},
	{"okhttp", "okhttp"},
	{"rust", "rust-http"},
	{"postman", "postman"},
	{"insomnia", "insomnia"},
	{"httpie", "httpie"},
	{"npm/", "npm"},
	{"pnpm", "pnpm"},
	{"yarn/", "yarn"},
	{"deno", "deno"},
	{"grcurl", "grpcurl"},
	{"k6", "k6"},
	{"chrome", "browser-chromium"},
	{"chromium", "browser-chromium"},
	{"edg/", "browser-edge"},
	{"edge", "browser-edge"},
	{"firefox", "browser-firefox"},
	{"fxios", "browser-firefox"},
	{"safari", "browser-webkit"},
	{"webkit", "browser-webkit"},
	{"mozilla", "mozilla"},
}

// normalizeUserAgent prepares a User-Agent for storage. With UA optimization
// enabled it collapses known clients to a compact token; unrecognized strings
// are kept or dropped depending on uaSaveUnknown. The result is always bounded
// by TruncateUserAgent. Optimization is intentionally cheap: a single lowercase
// pass and substring scans at flush time.
func (w *Writer) normalizeUserAgent(ua string) string {
	if !w.uaOptimize {
		return TruncateUserAgent(ua)
	}
	if ua == "" {
		return ""
	}
	lower := strings.ToLower(ua)
	for _, c := range canonicalUAs {
		if strings.Contains(lower, c.needle) {
			return c.token
		}
	}
	if !w.uaSaveUnknown {
		return ""
	}
	return TruncateUserAgent(ua)
}

// dictionaryID returns the row id for value in table, inserting the row on
// first sight and caching it for the process lifetime.
func (w *Writer) dictionaryID(ctx context.Context, tx *sql.Tx, table, value string, cache map[string]int64) (int64, error) {
	if id, ok := cache[value]; ok {
		return id, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE name = ?", value).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if err == nil {
		cache[value] = id
		return id, nil
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+" (name) VALUES (?)", value); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE name = ?", value).Scan(&id); err != nil {
		return 0, err
	}
	cache[value] = id
	return id, nil
}

// prune deletes rows older than the retention window and reclaims freed pages
// so the file stays dense. Best effort: failures are logged and retried on the
// next maintenance pass.
func (w *Writer) prune(now time.Time) {
	if w.retention <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	cutoff := now.Add(-w.retention).UnixMilli()
	if _, err := w.db.ExecContext(ctx, "DELETE FROM request_log WHERE ts < ?", cutoff); err != nil {
		w.logger.Warn("request log prune failed", "error", err)
		return
	}
	// Fold the WAL so freed pages land in the main file, then reclaim a
	// bounded chunk of free pages per pass without stalling writes for long.
	_, _ = w.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if _, err := w.db.ExecContext(ctx, "PRAGMA incremental_vacuum("+strconv.Itoa(vacuumChunk)+")"); err != nil {
		w.logger.Warn("request log vacuum failed", "error", err)
	}
}

func (w *Writer) pruneLoop() {
	defer close(w.pruneDone)
	if w.retention <= 0 {
		return
	}
	w.prune(time.Now())
	ticker := time.NewTicker(pruneEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.prune(time.Now())
		case <-w.stop:
			return
		}
	}
}

// Close flushes queued records, stops the maintenance loop, and closes the
// database. It is safe to call multiple times.
func (w *Writer) Close() error {
	var err error
	w.stopOnce.Do(func() {
		close(w.stop)
		<-w.done
		<-w.pruneDone
		err = w.db.Close()
	})
	return err
}

func (w *Writer) writeFailure(err error) {
	failures := w.writeFailures.Add(1)
	if failures <= 3 || failures%100 == 0 {
		w.logger.Warn("request log write failed", "error", err)
	}
}

// TruncateParams bounds a raw query string to maxParamsLen bytes without
// splitting a UTF-8 rune. Client-supplied query values are unbounded, so they
// are always truncated before being stored or logged.
func TruncateParams(value string) string {
	if len(value) <= maxParamsLen {
		return value
	}
	value = value[:maxParamsLen]
	for !utf8.ValidString(value) {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

// TruncateUserAgent bounds a User-Agent header to maxUserAgentLen bytes without
// splitting a UTF-8 rune. The header is unbounded client input, so it is always
// truncated before being stored.
func TruncateUserAgent(value string) string {
	if len(value) <= maxUserAgentLen {
		return value
	}
	value = value[:maxUserAgentLen]
	for !utf8.ValidString(value) {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
