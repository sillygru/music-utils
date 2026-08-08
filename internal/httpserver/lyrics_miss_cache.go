package httpserver

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	lyricsMissCacheTTL        = 24 * time.Hour
	lyricsMissCacheMaxEntries = 100000
)

// lyricsMissCache memoizes lyrics lookups that returned no result upstream so
// repeated misses stop spending LRCLIB budget for the TTL window. It is
// in-memory only: entries are bounded by a hard cap and expired lazily on
// lookup, so memory stays flat even under hostile traffic.
type lyricsMissCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func newLyricsMissCache() *lyricsMissCache {
	return &lyricsMissCache{entries: make(map[string]time.Time)}
}

// Has reports whether a miss for key is still cached at now. Expired entries
// are removed on read.
func (c *lyricsMissCache) Has(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	expires, ok := c.entries[key]
	if !ok {
		return false
	}
	if now.After(expires) {
		delete(c.entries, key)
		return false
	}
	return true
}

// Set records a miss for key until now+TTL. When the cache is full, one
// arbitrary entry is evicted so memory stays bounded.
func (c *lyricsMissCache) Set(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	if len(c.entries) >= lyricsMissCacheMaxEntries {
		for evict := range c.entries {
			delete(c.entries, evict)
			break
		}
	}
	c.entries[key] = now.Add(lyricsMissCacheTTL)
}

// lyricsMissKey builds a cache key that collapses durations to a 2-second
// stride so provider second-level rounding does not split an identical track
// into separate miss entries.
func lyricsMissKey(trackName, artistName, albumName string, duration float64) string {
	return strings.ToLower(strings.TrimSpace(trackName)) + "\x00" +
		strings.ToLower(strings.TrimSpace(artistName)) + "\x00" +
		strings.ToLower(strings.TrimSpace(albumName)) + "\x00" +
		lyricsMissDurationBucket(duration)
}

func lyricsMissDurationBucket(duration float64) string {
	if duration <= 0 {
		return "*"
	}
	return strconv.Itoa(int(duration/2) * 2)
}
