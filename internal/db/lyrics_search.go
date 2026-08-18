package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FindLyricsSearchCache returns a cached search response when it is newer than
// maxAge. Search cache entries are keyed by the canonical encoded query,
// including limit and rich-sync flags.
func FindLyricsSearchCache(ctx context.Context, database *sql.DB, key string, maxAge time.Duration) ([]byte, error) {
	if database == nil {
		return nil, errors.New("lyrics database is nil")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("lyrics search cache key is empty")
	}
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-maxAge).Format("2006-01-02 15:04:05")
	var response string
	err := database.QueryRowContext(ctx, `SELECT response_json FROM lyrics_search_cache WHERE cache_key=? AND updated_at >= ?`, key, cutoff).Scan(&response)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("find lyrics search cache: %w", err)
	}
	return []byte(response), nil
}

// UpsertLyricsSearchCache stores the complete JSON response for one canonical
// lyrics search query. The cache is deliberately separate from track rows so
// search results remain stable across release variants and rich-sync misses.
func UpsertLyricsSearchCache(ctx context.Context, database *sql.DB, key string, response []byte) error {
	if database == nil {
		return errors.New("lyrics database is nil")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("lyrics search cache key is empty")
	}
	if len(response) == 0 {
		return errors.New("lyrics search cache response is empty")
	}
	_, err := database.ExecContext(ctx, `INSERT INTO lyrics_search_cache (cache_key,response_json,updated_at)
VALUES (?,?,CURRENT_TIMESTAMP)
ON CONFLICT(cache_key) DO UPDATE SET response_json=excluded.response_json, updated_at=CURRENT_TIMESTAMP`, key, string(response))
	if err != nil {
		return fmt.Errorf("upsert lyrics search cache: %w", err)
	}
	return nil
}
