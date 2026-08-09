package cover

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned when no provider can resolve artwork.
var ErrNotFound = errors.New("cover not found in any provider")

const (
	positiveCacheTTL = time.Hour
	negativeCacheTTL = 24 * time.Hour
)

type cacheEntry struct {
	result    *Result
	persisted bool // set by handlers, not by the resolver
	notFound  bool
	expiresAt time.Time
}

// Resolver chains providers in order and memoizes both positive results and
// not-found misses so repeated lookups stop re-hitting upstream sources.
type Resolver struct {
	providers []Provider
	mu        sync.Mutex
	cache     map[string]cacheEntry
	search    map[string]searchCacheEntry
}

type searchCacheEntry struct {
	results   []Result
	expiresAt time.Time
}

// NewResolver builds a resolver over the given providers (nil entries are
// skipped). Order is the fallback order.
func NewResolver(providers ...Provider) *Resolver {
	return &Resolver{
		providers: providers,
		cache:     make(map[string]cacheEntry),
		search:    make(map[string]searchCacheEntry),
	}
}

func cacheKey(kind Kind, input Input) string {
	return kind.String() + "\x00" + normalize(input.TrackName) + "\x00" + normalize(input.ArtistName) + "\x00" + normalize(input.AlbumName)
}

// Search asks every configured provider for its top result and returns those
// results in provider order. Unlike Lookup, it intentionally does not stop at
// the first provider so callers can show provenance from multiple APIs.
func (r *Resolver) Search(ctx context.Context, kind Kind, input Input, limit int) ([]Result, error) {
	if limit < 1 {
		return []Result{}, nil
	}
	key := cacheKey(kind, input)
	r.mu.Lock()
	if entry, ok := r.search[key]; ok && time.Now().Before(entry.expiresAt) {
		results := append([]Result(nil), entry.results...)
		r.mu.Unlock()
		if len(results) > limit {
			results = results[:limit]
		}
		return results, nil
	}
	r.mu.Unlock()

	results := make([]Result, 0, len(r.providers))
	for _, provider := range r.providers {
		if provider == nil {
			continue
		}
		result, err := provider.Lookup(ctx, kind, input)
		if err != nil || result == nil || result.URL == "" {
			continue
		}
		result.TrackName = firstNonEmpty(result.TrackName, input.TrackName)
		result.ArtistName = firstNonEmpty(result.ArtistName, input.ArtistName)
		result.AlbumName = firstNonEmpty(result.AlbumName, input.AlbumName)
		results = append(results, *result)
	}
	r.mu.Lock()
	r.search[key] = searchCacheEntry{results: append([]Result(nil), results...), expiresAt: time.Now().Add(positiveCacheTTL)}
	r.mu.Unlock()
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// Lookup walks the providers in order and returns the first non-empty URL. A
// miss is memoized as a negative cache entry.
func (r *Resolver) Lookup(ctx context.Context, kind Kind, input Input) (*Result, error) {
	key := cacheKey(kind, input)
	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && time.Now().Before(entry.expiresAt) {
		r.mu.Unlock()
		if entry.notFound {
			return nil, ErrNotFound
		}
		return entry.result, nil
	}
	r.mu.Unlock()

	for _, provider := range r.providers {
		if provider == nil {
			continue
		}
		result, err := provider.Lookup(ctx, kind, input)
		if err == nil {
			r.store(key, result, false)
			return result, nil
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		// Transient/provider error without a result: keep trying the next tier
		// rather than recording a miss.
	}

	r.store(key, nil, true)
	return nil, ErrNotFound
}

func (r *Resolver) store(key string, result *Result, notFound bool) {
	ttl := negativeCacheTTL
	if !notFound {
		ttl = positiveCacheTTL
	}
	r.mu.Lock()
	r.cache[key] = cacheEntry{result: result, notFound: notFound, expiresAt: time.Now().Add(ttl)}
	r.mu.Unlock()
}
