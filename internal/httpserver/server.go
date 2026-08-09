package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/metadata"
	"github.com/sillygru/music-utils/internal/pacer"
	"github.com/sillygru/music-utils/internal/reqlog"
	"github.com/sillygru/music-utils/internal/version"
)

type requestStateKey struct{}

type requestState struct {
	outcome    string
	level      slog.Level
	detail     string
	cacheMs    int64
	upstreamMs int64
}

type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	state       *requestState
}

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *responseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// SetOutcome allows handlers and middleware to provide one request-level
// outcome to the outer logging middleware.
func (w *responseWriter) SetOutcome(outcome string) {
	if w.state != nil && outcome != "" {
		w.state.outcome = outcome
	}
}

func setOutcome(r *http.Request, outcome string) {
	if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
		state.outcome = outcome
	}
}

func setRequestIssue(r *http.Request, level slog.Level, detail string) {
	if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
		state.level = level
		state.detail = detail
	}
}

// setCacheDuration accumulates the time a handler spent in the local database
// or in-memory cache before any upstream fallback.
func setCacheDuration(r *http.Request, d time.Duration) {
	if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
		state.cacheMs += wholeMilliseconds(d)
	}
}

// setUpstreamDuration accumulates the time a handler spent querying upstream
// providers after a cache miss.
func setUpstreamDuration(r *http.Request, d time.Duration) {
	if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
		state.upstreamMs += wholeMilliseconds(d)
	}
}

// wholeMilliseconds reports d in whole milliseconds. A positive sub-millisecond
// duration is rounded up to 1 so a lookup that actually happened is never
// recorded as 0ms in the request log.
func wholeMilliseconds(d time.Duration) int64 {
	if ms := d.Milliseconds(); ms > 0 || d <= 0 {
		return ms
	}
	return 1
}

// New creates the HTTP server with the default application configuration.
// NewWithConfig should be used by the application when environment-based
// configuration is available. Cover artwork uses an in-memory database.
func New(port string, metadataDB, lyricsDB *sql.DB) *http.Server {
	return NewWithConfig(config.Config{Port: port}, metadataDB, lyricsDB)
}

// NewWithConfig creates the HTTP server and registers all application routes.
// Cover artwork uses an in-memory database absent an explicit cover DB.
func NewWithConfig(cfg config.Config, metadataDB, lyricsDB *sql.DB) *http.Server {
	server, _ := NewWithLogger(cfg, metadataDB, lyricsDB, nil, slog.Default())
	return server
}

// NewWithLogger is the injectable constructor used by the application and
// tests that need deterministic logging or a custom LRCLIB endpoint. It
// returns the server and the request-log writer, which is nil when request
// logging is disabled. The writer is also registered with
// RegisterOnShutdown, but Shutdown does not wait for those callbacks, so
// callers that need the final records flushed (the application on graceful
// shutdown, tests) must call Close themselves.
func NewWithLogger(cfg config.Config, metadataDB, lyricsDB, coverDB *sql.DB, logger *slog.Logger) (*http.Server, *reqlog.Writer) {
	if logger == nil {
		logger = slog.Default()
	}
	client := newLRCLIBClient(cfg, logger)
	var requestLogs *reqlog.Writer
	if cfg.RequestLogEnabled {
		var err error
		requestLogs, err = reqlog.Open(cfg.RequestLogDBPath, time.Duration(cfg.RequestLogRetentionDays)*24*time.Hour, logger)
		if err != nil {
			logger.Error("open request log database", "error", err)
			requestLogs = nil
		}
	}
	// iTunes is consumed by both the metadata and cover resolvers; share one
	// pacer so their combined traffic never exceeds iTunes' ~20 calls/min cap.
	itunesPace := pacer.New(2 * time.Second)
	metadataResolver := newMetadataResolver(cfg, logger, itunesPace)
	coverResolver := newCoverResolver(cfg, logger, itunesPace)
	if coverDB == nil {
		var err error
		coverDB, err = coverDatabase()
		if err != nil {
			logger.Error("open in-memory cover database", "error", err)
			coverDB = &sql.DB{}
		}
	}

	lyricsMisses := newLyricsMissCache()
	fallbacks := newFallbackGuard(cfg)
	coverRefresher := newCoverRefreshJob(cfg, coverDB, coverResolver, logger)
	prefetcher := newPrefetcher(cfg, metadataDB, lyricsDB, coverDB, coverResolver, client, lyricsMisses, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /version", versionHandler)
	mux.HandleFunc("GET /api/lyrics/get", getLyricsHandler(metadataDB, lyricsDB, client, lyricsMisses, fallbacks, cfg.LRCLIBFallbackEnabled, prefetcher))
	mux.HandleFunc("GET /api/lyrics/search", searchLyricsHandlerWithUpstream(metadataDB, lyricsDB, client, fallbacks, cfg.LRCLIBFallbackEnabled))
	mux.HandleFunc("GET /api/metadata/get", getMetadataHandler(metadataDB, metadataResolver, fallbacks, cfg.MetadataFallbackEnabled, prefetcher))
	mux.HandleFunc("GET /api/metadata/search", searchMetadataHandlerWithUpstream(metadataDB, metadataResolver, fallbacks, cfg.MetadataFallbackEnabled))
	mux.HandleFunc("GET /api/cover/get", getCoverTopHandler(metadataDB, coverDB, coverResolver, fallbacks, cfg.CoverFallbackEnabled, prefetcher))
	mux.HandleFunc("GET /api/cover/artist", getEntityCoverSearchHandler(coverDB, coverResolver, fallbacks, db.CoverArtist, cfg.CoverFallbackEnabled))
	mux.HandleFunc("GET /api/cover/album", getEntityCoverSearchHandler(coverDB, coverResolver, fallbacks, db.CoverAlbum, cfg.CoverFallbackEnabled))
	mux.HandleFunc("GET /api/cover/search", searchCoverHandler(metadataResolver, coverResolver, fallbacks, cfg.CoverFallbackEnabled))

	limiter := newRateLimiter(cfg)
	// The response cache replays identical requests (by method, path, and query)
	// without re-touching the database or upstream, so a burst of identical
	// messages from one client cannot hammer the DB. It is registered for
	// shutdown below so its sweeper goroutine cannot leak.
	replayCache := newResponseCache(responseReplayTTL)
	// CORS wraps the limiter so every response (including 429/503) carries the
	// headers browsers need, and preflight requests are answered before they
	// can consume rate-limit budget. The replay cache sits inside the limiter
	// and peers the mux's handlers so real API responses are deduplicated.
	application := recoverMiddleware(corsMiddleware(limiter.Handler(replayCache.middleware(mux))), logger)
	server := &http.Server{
		Addr:              ":" + normalizedPort(cfg.Port),
		Handler:           requestLogger(application, logger, requestLogs),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// WriteTimeout must cover the upstream queue wait (up to
		// FALLBACK_QUEUE_WAIT_MS) plus the slowest upstream call
		// (METADATA_TIMEOUT_MS / COVER_TIMEOUT_MS), or queued cache-missing
		// requests are killed mid-flight before their upstream lookup returns.
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	server.RegisterOnShutdown(limiter.Stop)
	server.RegisterOnShutdown(replayCache.Stop)
	server.RegisterOnShutdown(fallbacks.Stop)
	server.RegisterOnShutdown(coverRefresher.Stop)
	if prefetcher != nil {
		server.RegisterOnShutdown(prefetcher.Stop)
	}
	if requestLogs != nil {
		server.RegisterOnShutdown(func() { _ = requestLogs.Close() })
	}
	return server, requestLogs
}

func newCoverResolver(cfg config.Config, logger *slog.Logger, itunesPace *pacer.Pacer) *cover.Resolver {
	timeout := time.Duration(cfg.CoverTimeoutMS) * time.Millisecond
	userAgent := cfg.CoverUserAgent
	var providers []cover.Provider

	if lastfm, err := cover.NewLastfm(cfg.LastfmBaseURL, userAgent, timeout); err != nil {
		logger.Error("configure Last.fm cover provider", "error", err)
	} else {
		providers = append(providers, lastfm)
	}
	if itunes, err := cover.NewITunes(cfg.ITunesBaseURL, userAgent, timeout, itunesPace); err != nil {
		logger.Error("configure iTunes cover provider", "error", err)
	} else {
		providers = append(providers, itunes)
	}
	if deezer, err := cover.NewDeezer(cfg.DeezerBaseURL, userAgent, timeout); err != nil {
		logger.Error("configure Deezer cover provider", "error", err)
	} else {
		providers = append(providers, deezer)
	}
	if len(providers) == 0 {
		return nil
	}
	return cover.NewResolver(providers...)
}

// coverDatabase opens an in-memory cover schema for callers that pass no
// explicit cover DB (tests, New/NewWithConfig).
func coverDatabase() (*sql.DB, error) {
	database, err := db.Open(":memory:", db.Config{MmapSize: 1024 * 1024, CacheSizeKB: -2, MaxOpenConns: 1})
	if err != nil {
		return nil, err
	}
	if err := db.MigrateCover(context.Background(), database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func newMetadataResolver(cfg config.Config, logger *slog.Logger, itunesPace *pacer.Pacer) *metadata.Resolver {
	timeout := time.Duration(cfg.MetadataTimeoutMS) * time.Millisecond
	userAgent := cfg.MetadataUserAgent
	var providers []metadata.Provider

	if itunes, err := metadata.NewITunes(cfg.ITunesBaseURL, userAgent, timeout, itunesPace); err != nil {
		logger.Error("configure iTunes provider", "error", err)
	} else {
		providers = append(providers, itunes)
	}
	if deezer, err := metadata.NewDeezer(cfg.DeezerBaseURL, userAgent, timeout); err != nil {
		logger.Error("configure Deezer provider", "error", err)
	} else {
		providers = append(providers, deezer)
	}
	if len(providers) == 0 {
		return nil
	}
	return metadata.NewResolver(providers...)
}

func newLRCLIBClient(cfg config.Config, logger *slog.Logger) *lrclib.Client {
	baseURL := cfg.LRCLIBBaseURL
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://lrclib.net/api"
	}
	userAgent := cfg.LRCLIBUserAgent
	if strings.TrimSpace(userAgent) == "" {
		userAgent = "music-utils/" + version.Version + " (+https://gru0.dev)"
	}
	timeout := time.Duration(cfg.LRCLIBTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client, err := lrclib.New(baseURL, userAgent, timeout)
	if err != nil {
		logger.Error("configure LRCLIB client", "error", err)
		return nil
	}
	return client
}

func normalizedPort(port string) string {
	if strings.TrimSpace(port) == "" {
		return "8080"
	}
	return strings.TrimSpace(port)
}

func requestLogger(next http.Handler, logger *slog.Logger, logs *reqlog.Writer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &requestState{outcome: "error", level: slog.LevelInfo}
		ctx := contextWithRequestState(r, state)
		started := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, state: state}
		next.ServeHTTP(wrapped, r.WithContext(ctx))
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode(),
			"duration_ms", time.Since(started).Seconds() * 1000,
			"client_ip", clientIP(r, false),
			"outcome", state.outcome,
		}
		if logs != nil {
			// Request logging is enabled: surface params and the split
			// cache/upstream timings in the application log and persist a row
			// to the request log database.
			if params := reqlog.TruncateParams(r.URL.RawQuery); params != "" {
				attrs = append(attrs, "params", params)
			}
			if state.cacheMs > 0 {
				attrs = append(attrs, "cache_ms", state.cacheMs)
			}
			if state.upstreamMs > 0 {
				attrs = append(attrs, "upstream_ms", state.upstreamMs)
			}
			logs.Log(reqlog.Record{
				TS:         started,
				Method:     r.Method,
				Endpoint:   r.URL.Path,
				Status:     wrapped.statusCode(),
				Outcome:    state.outcome,
				CacheMs:    state.cacheMs,
				UpstreamMs: state.upstreamMs,
				Params:     r.URL.RawQuery,
			})
		}
		if state.detail != "" {
			attrs = append(attrs, "detail", state.detail)
		}
		logger.Log(r.Context(), state.level, "request", attrs...)
	})
}

// corsMiddleware makes the API callable directly from browsers. It sets
// permissive CORS headers on every response and answers OPTIONS preflight
// requests with 204 before they reach the rate limiter.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			setOutcome(r, "preflight")
			header.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			header.Set("Access-Control-Allow-Headers", "*")
			header.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func recoverMiddleware(next http.Handler, _ *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				setOutcome(r, "error")
				setRequestIssue(r, slog.LevelError, fmt.Sprintf("panic: %v; stack: %s", recovered, debug.Stack()))
				if writer, ok := w.(*responseWriter); ok && writer.wroteHeader {
					return
				}
				writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func contextWithRequestState(r *http.Request, state *requestState) context.Context {
	return context.WithValue(r.Context(), requestStateKey{}, state)
}

func versionHandler(w http.ResponseWriter, r *http.Request) {
	setOutcome(r, "local_hit")
	writeJSON(w, http.StatusOK, struct {
		Version string `json:"version"`
	}{Version: version.Version})
}

func healthz(w http.ResponseWriter, r *http.Request) {
	setOutcome(r, "local_hit")
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
