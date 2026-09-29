package metadata

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

// TestResolverReportsTransientFailureAsInconclusive pins the distinction the
// backfill depends on: "we could not find out" must not look like "this song
// has no match".
func TestResolverReportsTransientFailureAsInconclusive(t *testing.T) {
	provider := &stubProvider{name: "p", err: errors.New("iTunes returned HTTP 500")}
	resolver := NewResolver(provider)

	_, err := resolver.Lookup(context.Background(), Input{TrackName: "Song", ArtistName: "Artist"})
	if err == nil {
		t.Fatal("expected an error when every provider fails transiently")
	}
	if !errors.Is(err, ErrInconclusive) {
		t.Errorf("a provider error must be reported as inconclusive, got %v", err)
	}
	// The server still answers this as a miss, exactly as before, so existing
	// HTTP behaviour is unchanged.
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("an inconclusive result must still satisfy ErrNotFound, got %v", err)
	}
	if IsRateLimited(err) {
		t.Error("a provider error must not be classified as rate limiting")
	}
}

// TestResolverDoesNotCacheTransientFailureAsMiss is the data-integrity case: a
// momentary 500 must not permanently record that a song has no upstream data.
func TestResolverDoesNotCacheTransientFailureAsMiss(t *testing.T) {
	provider := &stubProvider{name: "p", err: errors.New("iTunes returned HTTP 503")}
	resolver := NewResolver(provider)

	input := Input{TrackName: "Song", ArtistName: "Artist"}
	for i := 0; i < 3; i++ {
		if _, err := resolver.Lookup(context.Background(), input); !errors.Is(err, ErrInconclusive) {
			t.Fatalf("attempt %d: expected an inconclusive result, got %v", i+1, err)
		}
	}
	if provider.call.Load() != 3 {
		t.Fatalf("expected every attempt to reach the provider, got %d calls", provider.call.Load())
	}

	// Once the provider recovers the next lookup must succeed rather than being
	// served from a negative cache entry.
	provider.err = nil
	provider.track = &db.Track{Name: "Song", ArtistName: "Artist"}
	track, err := resolver.Lookup(context.Background(), input)
	if err != nil {
		t.Fatalf("expected recovery after the provider healed, got %v", err)
	}
	if track == nil || track.Name != "Song" {
		t.Fatalf("unexpected track after recovery: %+v", track)
	}
}

// TestResolverStillCachesGenuineMiss confirms the fix did not disable real
// negative caching, which is what stops the job re-fetching the same dead song
// on every future run.
func TestResolverStillCachesGenuineMiss(t *testing.T) {
	provider := &stubProvider{name: "p", err: ErrNotFound}
	resolver := NewResolver(provider)

	input := Input{TrackName: "Missing", ArtistName: "Artist"}
	for i := 0; i < 3; i++ {
		_, err := resolver.Lookup(context.Background(), input)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("attempt %d: expected ErrNotFound, got %v", i+1, err)
		}
		if errors.Is(err, ErrInconclusive) {
			t.Fatalf("a genuine miss must not be reported as inconclusive: %v", err)
		}
	}
	if provider.call.Load() != 1 {
		t.Fatalf("expected a genuine miss to be cached, got %d calls", provider.call.Load())
	}
}

// TestResolverRecoversWhenOneProviderFails covers the mixed case: one provider
// is broken, another answers. The track is found, so nothing is marked.
func TestResolverRecoversWhenOneProviderFails(t *testing.T) {
	broken := &stubProvider{name: "broken", err: errors.New("dial tcp: connection refused")}
	working := &stubProvider{name: "working", track: &db.Track{Name: "Song", ArtistName: "Artist"}}
	resolver := NewResolver(broken, working)

	track, err := resolver.Lookup(context.Background(), Input{TrackName: "Song", ArtistName: "Artist"})
	if err != nil {
		t.Fatalf("expected the healthy provider to resolve, got %v", err)
	}
	if track == nil || track.Name != "Song" {
		t.Fatalf("unexpected track: %+v", track)
	}
}

// TestResolverRateLimitStillWinsOverInconclusive makes sure a throttle is still
// recognised when a later provider also fails outright, so the job keeps
// backing off rather than treating the throttle as a generic error.
func TestResolverRateLimitStillWinsOverInconclusive(t *testing.T) {
	throttled := &stubProvider{name: "throttled", err: &RateLimitError{Provider: "throttled", Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Second}}
	broken := &stubProvider{name: "broken", err: errors.New("connection reset")}
	resolver := NewResolver(throttled, broken)

	_, err := resolver.Lookup(context.Background(), Input{TrackName: "Song", ArtistName: "Artist"})
	if !IsRateLimited(err) {
		t.Errorf("expected the throttle to remain detectable, got %v", err)
	}
	if got := RetryAfterFor(err); got != 2*time.Second {
		t.Errorf("expected the advertised cooldown to survive, got %s", got)
	}
	if !errors.Is(err, ErrInconclusive) {
		t.Errorf("a throttled lookup must also be inconclusive, got %v", err)
	}
}
