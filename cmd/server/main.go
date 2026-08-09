package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/httpserver"
	"github.com/sillygru/music-utils/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "export" {
		os.Exit(runExport(os.Args[2:]))
	}
	cfg, err := config.LoadAndValidate()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logger := newLogger(cfg.LogLevel)
	logger.Info("configuration loaded",
		"version", version.Version,
		"port", cfg.Port,
		"metadata_db_path", cfg.MetadataDBPath,
		"lyrics_db_path", cfg.LyricsDBPath,
		"cover_db_path", cfg.CoverDBPath,
		"db_max_open_conns", cfg.DBMaxOpenConns,
		"rate_limit_per_sec", cfg.RateLimitPerSec,
		"rate_limit_per_min", cfg.RateLimitPerMin,
		"fallback_per_min", cfg.FallbackPerMin,
		"fallback_max_queue", cfg.FallbackMaxQueue,
		"fallback_queue_wait_ms", cfg.FallbackQueueWaitMS,
		"trust_proxy", cfg.TrustProxy,
		"lrclib_fallback_enabled", cfg.LRCLIBFallbackEnabled,
		"lrclib_base_url", cfg.LRCLIBBaseURL,
		"lrclib_timeout_ms", cfg.LRCLIBTimeoutMS,
		"metadata_fallback_enabled", cfg.MetadataFallbackEnabled,
		"cover_fallback_enabled", cfg.CoverFallbackEnabled,
		"itunes_base_url", cfg.ITunesBaseURL,
		"deezer_base_url", cfg.DeezerBaseURL,
		"lastfm_base_url", cfg.LastfmBaseURL,
		"cover_timeout_ms", cfg.CoverTimeoutMS,
		"metadata_timeout_ms", cfg.MetadataTimeoutMS,
		"cover_refresh_enabled", cfg.CoverRefreshEnabled,
		"cover_refresh_after_days", cfg.CoverRefreshAfterDays,
		"cover_refresh_start_hour", cfg.CoverRefreshStartHour,
		"cover_refresh_end_hour", cfg.CoverRefreshEndHour,
		"request_log_enabled", cfg.RequestLogEnabled,
		"request_log_db_path", cfg.RequestLogDBPath,
		"request_log_retention_days", cfg.RequestLogRetentionDays,
	)

	metadataDB, err := db.Open(cfg.MetadataDBPath, db.Config{
		MmapSize:     cfg.DBMmapSize,
		CacheSizeKB:  cfg.DBCacheSizeKB,
		MaxOpenConns: cfg.DBMaxOpenConns,
	})
	if err != nil {
		logger.Error("open metadata database", "error", err)
		os.Exit(1)
	}
	defer metadataDB.Close()

	lyricsDB, err := db.Open(cfg.LyricsDBPath, db.Config{
		MmapSize:     cfg.DBMmapSize,
		CacheSizeKB:  cfg.DBCacheSizeKB,
		MaxOpenConns: cfg.DBMaxOpenConns,
	})
	if err != nil {
		logger.Error("open lyrics database", "error", err)
		os.Exit(1)
	}
	defer lyricsDB.Close()

	coverDB, err := db.Open(cfg.CoverDBPath, db.Config{
		MmapSize:     cfg.DBMmapSize,
		CacheSizeKB:  cfg.DBCacheSizeKB,
		MaxOpenConns: cfg.DBMaxOpenConns,
	})
	if err != nil {
		logger.Error("open cover database", "error", err)
		os.Exit(1)
	}
	defer coverDB.Close()

	ctx := context.Background()
	if err := db.MigrateMetadata(ctx, metadataDB); err != nil {
		logger.Error("migrate metadata database", "error", err)
		os.Exit(1)
	}
	if err := db.MigrateLyrics(ctx, lyricsDB); err != nil {
		logger.Error("migrate lyrics database", "error", err)
		os.Exit(1)
	}
	if err := db.MigrateCover(ctx, coverDB); err != nil {
		logger.Error("migrate cover database", "error", err)
		os.Exit(1)
	}
	if err := db.MigrateLegacyLyrics(ctx, metadataDB, lyricsDB); err != nil {
		logger.Error("migrate legacy lyrics data", "error", err)
		os.Exit(1)
	}

	server, requestLogs := httpserver.NewWithLogger(cfg, metadataDB, lyricsDB, coverDB, logger)
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("server listening", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(shutdown)

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
		}
	case <-shutdown:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		} else {
			logger.Info("server stopped")
		}
	}
	// Flush the queued request log before the process exits. Shutdown does not
	// wait for RegisterOnShutdown hooks (and does not run them on timeout), so
	// Close must be called explicitly on every exit path.
	if requestLogs != nil {
		if err := requestLogs.Close(); err != nil {
			logger.Error("close request log", "error", err)
		}
	}
}

func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel}))
}
