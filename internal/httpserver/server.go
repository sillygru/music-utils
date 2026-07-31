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
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/version"
)

type requestStateKey struct{}

type requestState struct {
	outcome string
	level   slog.Level
	detail  string
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

// New creates the HTTP server with the default application configuration.
// NewWithConfig should be used by the application when environment-based
// configuration is available.
func New(port string, database *sql.DB) *http.Server {
	return NewWithConfig(config.Config{Port: port}, database)
}

// NewWithConfig creates the HTTP server and registers all application routes.
func NewWithConfig(cfg config.Config, database *sql.DB) *http.Server {
	return NewWithLogger(cfg, database, slog.Default())
}

// NewWithLogger is the injectable constructor used by the application and
// tests that need deterministic logging or a custom LRCLIB endpoint.
func NewWithLogger(cfg config.Config, database *sql.DB, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.Default()
	}
	client := newLRCLIBClient(cfg, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /version", versionHandler)
	mux.HandleFunc("GET /api/get", getLyricsHandler(database, client, cfg.LRCLIBFallbackEnabled))
	mux.HandleFunc("GET /api/search", searchLyricsHandler(database))

	limiter := newRateLimiter(cfg)
	application := recoverMiddleware(limiter.Handler(mux), logger)
	server := &http.Server{
		Addr:              ":" + normalizedPort(cfg.Port),
		Handler:           requestLogger(application, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	server.RegisterOnShutdown(limiter.Stop)
	return server
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

func requestLogger(next http.Handler, logger *slog.Logger) http.Handler {
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
		if state.detail != "" {
			attrs = append(attrs, "detail", state.detail)
		}
		logger.Log(r.Context(), state.level, "request", attrs...)
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
