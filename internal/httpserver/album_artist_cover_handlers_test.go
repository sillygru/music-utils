package httpserver

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
)

type coverStubProvider struct {
	name   string
	result *cover.Result
	calls  int
}

func (c *coverStubProvider) Name() string { return c.name }
func (c *coverStubProvider) Lookup(_ context.Context, _ cover.Kind, _ cover.Input) (*cover.Result, error) {
	c.calls++
	if c.result == nil {
		return nil, cover.ErrNotFound
	}
	return c.result, nil
}

// coverSearchStub is a cover provider stub that returns multiple candidates so
// handler tests exercise the multi-result filter path.
type coverSearchStub struct {
	name    string
	results []cover.Result
	calls   int
}

func (c *coverSearchStub) Name() string { return c.name }
func (c *coverSearchStub) Lookup(_ context.Context, _ cover.Kind, _ cover.Input) (*cover.Result, error) {
	c.calls++
	if len(c.results) == 0 {
		return nil, cover.ErrNotFound
	}
	return &c.results[0], nil
}
func (c *coverSearchStub) Search(_ context.Context, _ cover.Kind, _ cover.Input, limit int) ([]cover.Result, error) {
	c.calls++
	out := make([]cover.Result, 0, len(c.results))
	for _, result := range c.results {
		if len(out) >= limit {
			break
		}
		out = append(out, result)
	}
	return out, nil
}

// testFallbackGuard returns a guard with generous limits so handler-level
// tests never trip the per-IP budget or the queue gate.
func testFallbackGuard() *fallbackGuard {
	return newFallbackGuard(config.Config{FallbackPerMin: 100, FallbackMaxQueue: 100})
}

func testCoverDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(":memory:", db.Config{MmapSize: 1024 * 1024, CacheSizeKB: -2, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open cover database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateCover(context.Background(), database); err != nil {
		t.Fatalf("migrate cover database: %v", err)
	}
	return database
}

func performArtistRequest(t *testing.T, handler http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
