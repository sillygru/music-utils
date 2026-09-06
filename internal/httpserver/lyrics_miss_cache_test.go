package httpserver

import (
	"fmt"
	"testing"
	"time"
)

func TestLyricsMissCacheExpiryAndKeyCollapse(t *testing.T) {
	cache := newLyricsMissCache()
	now := time.Now()

	key := lyricsMissKey("  Ghost Song ", "Artist", "Album")
	cache.Set(key, now)
	if !cache.Has(key, now) {
		t.Fatal("expected cached miss to be present")
	}
	// Duration is never part of the key: differing durations for the same
	// title/artist/album must hit the same entry.
	similar := lyricsMissKey("Ghost Song", "artist", "album")
	if !cache.Has(similar, now) {
		t.Fatal("expected durationless key to hit the same entry")
	}
	// A genuinely different key must miss.
	other := lyricsMissKey("Ghost Song", "Other Artist", "Album")
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
		cache.Set(lyricsMissKey("track", "artist", fmt.Sprintf("album-%d", i)), now)
	}
	cache.mu.Lock()
	size := len(cache.entries)
	cache.mu.Unlock()
	if size > lyricsMissCacheMaxEntries {
		t.Fatalf("cache grew past its bound: %d entries", size)
	}
}
