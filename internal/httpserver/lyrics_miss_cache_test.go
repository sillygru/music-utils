package httpserver

import (
	"testing"
	"time"
)

func TestLyricsMissCacheExpiryAndKeyCollapse(t *testing.T) {
	cache := newLyricsMissCache()
	now := time.Now()

	key := lyricsMissKey("  Ghost Song ", "Artist", "Album", 203.5)
	cache.Set(key, now)
	if !cache.Has(key, now) {
		t.Fatal("expected cached miss to be present")
	}
	// Duration is collapsed to a 2-second stride, so a slightly different
	// duration within the same stride must hit the same entry.
	similar := lyricsMissKey("Ghost Song", "artist", "album", 203.9)
	if !cache.Has(similar, now) {
		t.Fatal("expected duration-stride key to hit the same entry")
	}
	// A duration in a different stride must miss.
	otherStride := lyricsMissKey("Ghost Song", "Artist", "Album", 204)
	if cache.Has(otherStride, now) {
		t.Fatal("expected a different duration stride to miss the cache")
	}
	// A genuinely different key must miss.
	other := lyricsMissKey("Ghost Song", "Other Artist", "Album", 203.5)
	if cache.Has(other, now) {
		t.Fatal("expected a different artist to miss the cache")
	}
	// Entries expire after the TTL.
	if cache.Has(key, now.Add(lyricsMissCacheTTL+time.Second)) {
		t.Fatal("expected expired entry to be absent")
	}
}

func TestLyricsMissCacheBoundedWhenFull(t *testing.T) {
	cache := newLyricsMissCache()
	now := time.Now()
	for i := 0; i < lyricsMissCacheMaxEntries+10; i++ {
		cache.Set(lyricsMissKey("track", "artist", "", float64(i)), now)
	}
	cache.mu.Lock()
	size := len(cache.entries)
	cache.mu.Unlock()
	if size > lyricsMissCacheMaxEntries {
		t.Fatalf("cache grew past its bound: %d entries", size)
	}
}
