package metadata

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

const (
	positiveCacheTTL    = time.Hour
	negativeCacheTTL    = 24 * time.Hour
	durationCacheStride = 2 // seconds
)

type cachedEntry struct {
	value     *db.Track
	notFound  bool
	expiresAt time.Time
}

// Resolver chains providers in order and memoizes both positive results and
// not-found misses for a bounded lifetime so repeated lookups stop re-hitting
// upstream providers.
type Resolver struct {
	providers []Provider
	mu        sync.Mutex
	cache     map[string]cachedEntry
}

func NewResolver(providers ...Provider) *Resolver {
	return &Resolver{
		providers: providers,
		cache:     make(map[string]cachedEntry),
	}
}

func cacheKey(input Input) string {
	return normalize(input.TrackName) + "\x00" + normalize(input.ArtistName) + "\x00" + normalize(input.AlbumName) + "\x00" + durationKey(input.Duration)
}

// Search queries every provider that supports multi-result search and merges
// the responses in provider order. Duplicate tracks are kept once, while the
// first provider's metadata remains authoritative for the merged item.
func (r *Resolver) Search(ctx context.Context, query string, limit int) ([]*db.Track, error) {
	if limit < 1 {
		return []*db.Track{}, nil
	}
	if limit > 50 {
		limit = 50
	}
	results := make([]*db.Track, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, provider := range r.providers {
		searchProvider, ok := provider.(SearchProvider)
		if !ok || searchProvider == nil {
			continue
		}
		tracks, err := searchProvider.Search(ctx, query, limit)
		if err != nil {
			continue
		}
		for _, track := range tracks {
			if track == nil {
				continue
			}
			key := normalize(track.Name) + "\x00" + normalize(track.ArtistName) + "\x00" + normalize(track.AlbumName)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			results = append(results, track)
		}
	}
	return results, nil
}

func (r *Resolver) Lookup(ctx context.Context, input Input) (*db.Track, error) {
	key := cacheKey(input)
	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && time.Now().Before(entry.expiresAt) {
		r.mu.Unlock()
		if entry.notFound {
			return nil, ErrNotFound
		}
		return entry.value, nil
	}
	r.mu.Unlock()

	for _, provider := range r.providers {
		if provider == nil {
			continue
		}
		track, err := provider.Lookup(ctx, input)
		if err == nil {
			r.store(key, track, false)
			return track, nil
		}
		if !errors.Is(err, ErrNotFound) {
			// Transient/provider error without a result: keep trying the next
			// tier rather than recording a miss.
			continue
		}
	}

	r.store(key, nil, true)
	return nil, ErrNotFound
}

func (r *Resolver) store(key string, track *db.Track, notFound bool) {
	ttl := negativeCacheTTL
	if !notFound {
		ttl = positiveCacheTTL
	}
	r.mu.Lock()
	r.cache[key] = cachedEntry{value: track, notFound: notFound, expiresAt: time.Now().Add(ttl)}
	r.mu.Unlock()
}

// durationKey collapses durations to a stride so provider second-level rounding
// does not split an identical track into separate lookup keys.
func durationKey(value float64) string {
	if value <= 0 {
		return "*"
	}
	return strconv.Itoa(int(value/durationCacheStride) * int(durationCacheStride))
}
