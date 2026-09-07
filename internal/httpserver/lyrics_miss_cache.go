package httpserver

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{6,16}$`)

// sanitizeVideoID normalizes an optional YouTube videoId hint. Invalid values
// are dropped (empty) rather than rejected so older clients keep working.
func sanitizeVideoID(value string) string {
	value = strings.TrimSpace(value)
	if !videoIDPattern.MatchString(value) {
		return ""
	}
	return value
}

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

// lyricsMissKey builds a cache key from title, artist, and album only.
// Duration is deliberately excluded: upstream lyrics lookups never receive it,
// so misses must not be split by it either.
func lyricsMissKey(trackName, artistName, albumName string) string {
	return lyricsMissKeyWithVideo(trackName, artistName, albumName, "")
}

// lyricsMissKeyWithVideo extends the miss key with the videoId hint so
// video-keyed lookups never share memoization with text-only lookups.
func lyricsMissKeyWithVideo(trackName, artistName, albumName, videoID string) string {
	return strings.ToLower(strings.TrimSpace(trackName)) + "\x00" +
		strings.ToLower(strings.TrimSpace(artistName)) + "\x00" +
		strings.ToLower(strings.TrimSpace(albumName)) + "\x00" +
		strings.ToLower(strings.TrimSpace(videoID))
}

// providerMissKey adds the provider dimension so each upstream is memoized independently.
func providerMissKey(provider, trackName, artistName, albumName, videoID string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" + lyricsMissKeyWithVideo(trackName, artistName, albumName, videoID)
}

// HasProvider reports whether a miss for provider+key is still cached.
func (c *lyricsMissCache) HasProvider(provider, trackName, artistName, albumName, videoID string, now time.Time) bool {
	return c.Has(providerMissKey(provider, trackName, artistName, albumName, videoID), now)
}

// SetProvider records a provider-scoped miss until now+TTL.
func (c *lyricsMissCache) SetProvider(provider, trackName, artistName, albumName, videoID string, now time.Time) {
	c.Set(providerMissKey(provider, trackName, artistName, albumName, videoID), now)
}
