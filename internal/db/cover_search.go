package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CoverSearchCacheTTL is how long a cached cover-search response is replayed
// before it is considered stale.
//
// It matches the lyrics search cache: a cover URL that a provider offered once
// does not go stale in a day, and re-asking for it is what puts a burst of
// identical searches into the upstream queue.
const CoverSearchCacheTTL = 24 * time.Hour

// FindCoverSearchCache returns a cached cover-search response when it is newer
// than maxAge. Entries are keyed by the canonical encoded query, including the
// entity kind and limit.
func FindCoverSearchCache(ctx context.Context, database *sql.DB, key string, maxAge time.Duration) ([]byte, error) {
	if database == nil {
		return nil, errors.New("cover database is nil")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("cover search cache key is empty")
	}
	if maxAge <= 0 {
		maxAge = CoverSearchCacheTTL
	}
	cutoff := time.Now().UTC().Add(-maxAge).Format("2006-01-02 15:04:05")
	var response string
	err := database.QueryRowContext(ctx, `SELECT response_json FROM cover_search_cache WHERE cache_key=? AND updated_at >= ?`, key, cutoff).Scan(&response)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("find cover search cache: %w", err)
	}
	return []byte(response), nil
}

// UpsertCoverSearchCache stores the complete JSON response for one canonical
// cover-search query.
//
// An empty result set is stored too, and that is the point: a query that has
// been asked and answered with nothing is the case most worth remembering,
// because without it every repeat of an unproductive search spends the full
// upstream budget to be told nothing again.
func UpsertCoverSearchCache(ctx context.Context, database *sql.DB, key string, response []byte) error {
	if database == nil {
		return errors.New("cover database is nil")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("cover search cache key is empty")
	}
	if len(response) == 0 {
		return errors.New("cover search cache response is empty")
	}
	_, err := database.ExecContext(ctx, `INSERT INTO cover_search_cache (cache_key,response_json,updated_at)
VALUES (?,?,CURRENT_TIMESTAMP)
ON CONFLICT(cache_key) DO UPDATE SET response_json=excluded.response_json, updated_at=CURRENT_TIMESTAMP`, key, string(response))
	if err != nil {
		return fmt.Errorf("upsert cover search cache: %w", err)
	}
	return nil
}