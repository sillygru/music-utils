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
	cfg, err := config.LoadAndValidate()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logger := newLogger(cfg.LogLevel)
	logger.Info("configuration loaded",
		"version", version.Version,
		"port", cfg.Port,
		"db_path", cfg.DBPath,
		"db_max_open_conns", cfg.DBMaxOpenConns,
		"rate_limit_per_sec", cfg.RateLimitPerSec,
		"rate_limit_per_min", cfg.RateLimitPerMin,
		"trust_proxy", cfg.TrustProxy,
		"lrclib_fallback_enabled", cfg.LRCLIBFallbackEnabled,
		"lrclib_base_url", cfg.LRCLIBBaseURL,
		"lrclib_timeout_ms", cfg.LRCLIBTimeoutMS,
	)

	database, err := db.Open(cfg.DBPath, db.Config{
		MmapSize:     cfg.DBMmapSize,
		CacheSizeKB:  cfg.DBCacheSizeKB,
		MaxOpenConns: cfg.DBMaxOpenConns,
	})
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := db.Migrate(context.Background(), database); err != nil {
		logger.Error("migrate database", "error", err)
		os.Exit(1)
	}

	server := httpserver.NewWithLogger(cfg, database, logger)
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
			return
		}
		logger.Info("server stopped")
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
