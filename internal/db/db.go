package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const defaultBusyTimeoutMS = 5000

// Config controls SQLite connection behavior.
type Config struct {
	MmapSize     int64
	CacheSizeKB  int64
	MaxOpenConns int
}

// Open opens a SQLite database, configures the connection pool, and verifies
// that the database can accept a connection. Schema creation is handled by
// Migrate so callers can control startup ordering explicitly.
func Open(path string, cfg Config) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("database path is empty")
	}
	if cfg.MmapSize <= 0 {
		return nil, fmt.Errorf("database mmap size must be positive")
	}
	if cfg.MaxOpenConns < 1 {
		return nil, fmt.Errorf("database max open connections must be positive")
	}

	if path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create database directory: %w", err)
			}
		}
	}

	dsn := sqliteDSN(path, cfg)
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	database.SetMaxOpenConns(cfg.MaxOpenConns)
	database.SetMaxIdleConns(cfg.MaxOpenConns)

	if err := database.PingContext(context.Background()); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}

	return database, nil
}

func sqliteDSN(path string, cfg Config) string {
	// SQLite has special URI forms for in-memory databases. Preserve them
	// rather than escaping them into literal filenames.
	base := "file:" + url.PathEscape(path)
	if path == ":memory:" {
		base = "file::memory:"
	} else if strings.HasPrefix(path, "file::memory:") {
		base = path
	}

	pragmas := fmt.Sprintf(
		"_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)&_pragma=mmap_size(%s)&_pragma=cache_size(%s)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(%d)",
		strconv.FormatInt(cfg.MmapSize, 10),
		strconv.FormatInt(cfg.CacheSizeKB, 10),
		defaultBusyTimeoutMS,
	)
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	return base + separator + pragmas
}
