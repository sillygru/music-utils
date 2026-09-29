package metadata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

// TestResolverSurfacesRateLimitingAsBothMissAndThrottle pins the contract the
// jobs backfill depends on: a throttled lookup must still look like a miss to
// existing callers, while remaining distinguishable from a real miss.
func TestResolverSurfacesRateLimitingAsBothMissAndThrottle(t *testing.T) {
	throttled := &RateLimitError{Provider: "itunes", Status: 429, RetryAfter: 3 * time.Second}
	provider := &stubProvider{name: "p", err: throttled}
	resolver := NewResolver(provider)

	_, err := resolver.Lookup(context.Background(), Input{TrackName: "Song", ArtistName: "Artist"})
	if err == nil {
		t.Fatal("expected an error when every provider is throttled")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("a throttled lookup must still satisfy ErrNotFound, got %v", err)
	}
	if !IsRateLimited(err) {
		t.Errorf("a throttled lookup must be detectable as rate limiting, got %v", err)
	}
	if got := RetryAfterFor(err); got != 3*time.Second {
		t.Errorf("expected the 3s cooldown to survive, got %s", got)
	}
}

// TestResolverDoesNotCacheThrottledLookupAsMiss is the important one: caching a
// throttled lookup as a 24h negative would suppress every later retry and let a
// backfill record a song as permanently resolved purely because the provider
// was busy at the time.
func TestResolverDoesNotCacheThrottledLookupAsMiss(t *testing.T) {
	throttled := &RateLimitError{Provider: "itunes", Status: 429}
	provider := &stubProvider{name: "p", err: throttled}
	resolver := NewResolver(provider)

	input := Input{TrackName: "Song", ArtistName: "Artist"}
	for i := 0; i < 3; i++ {
		if _, err := resolver.Lookup(context.Background(), input); !IsRateLimited(err) {
			t.Fatalf("attempt %d: expected rate limiting, got %v", i+1, err)
		}
	}
	if provider.call.Load() != 3 {
		t.Fatalf("expected every retry to reach the provider, got %d calls", provider.call.Load())
	}

	// Once the provider recovers, the very next lookup must succeed rather than
	// being served from a negative cache entry.
	provider.err = nil
	provider.track = &db.Track{Name: "Song", ArtistName: "Artist"}
	track, err := resolver.Lookup(context.Background(), input)
	if err != nil {
		t.Fatalf("expected recovery after throttling cleared, got %v", err)
	}
	if track == nil || track.Name != "Song" {
		t.Fatalf("unexpected track after recovery: %+v", track)
	}
}

// TestResolverCachesGenuineMiss confirms the real negative cache still works,
// so the throttling fix did not disable memoization altogether.
func TestResolverCachesGenuineMiss(t *testing.T) {
	provider := &stubProvider{name: "p", err: ErrNotFound}
	resolver := NewResolver(provider)

	input := Input{TrackName: "Missing", ArtistName: "Artist"}
	for i := 0; i < 3; i++ {
		if _, err := resolver.Lookup(context.Background(), input); !errors.Is(err, ErrNotFound) {
			t.Fatalf("attempt %d: expected ErrNotFound, got %v", i+1, err)
		} else if IsRateLimited(err) {
			t.Fatalf("a genuine miss must not be classified as rate limiting: %v", err)
		}
	}
	if provider.call.Load() != 1 {
		t.Fatalf("expected a genuine miss to be cached, got %d calls", provider.call.Load())
	}
}

// TestResolverFallsThroughToNextProviderWhenThrottled checks that one throttled
// provider does not prevent a later provider from resolving the track.
func TestResolverFallsThroughToNextProviderWhenThrottled(t *testing.T) {
	throttled := &stubProvider{name: "primary", err: &RateLimitError{Provider: "primary", Status: 429}}
	secondary := &stubProvider{name: "secondary", track: &db.Track{Name: "Song", ArtistName: "Artist"}}
	resolver := NewResolver(throttled, secondary)

	track, err := resolver.Lookup(context.Background(), Input{TrackName: "Song", ArtistName: "Artist"})
	if err != nil {
		t.Fatalf("expected the secondary provider to resolve, got %v", err)
	}
	if track == nil || track.Name != "Song" {
		t.Fatalf("unexpected track: %+v", track)
	}
}
