package metadata

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sillygru/music-utils/internal/upstream"
)

// The provider failure classification moved to internal/upstream so the lyrics
// providers classify their responses the same way. These aliases have to stay
// aliases rather than becoming defined types: errors.As matches on the concrete
// type, so a distinct type here would stop a metadata.RateLimitError from being
// recognized by shared classification code, and a batch job walking both
// provider families would classify the same failure differently depending on
// which package it came from.
func TestRateLimitAliasIsRecognizedBySharedClassification(t *testing.T) {
	built := &RateLimitError{Provider: "itunes", Status: http.StatusTooManyRequests}
	if !upstream.IsRateLimited(built) {
		t.Error("a metadata.RateLimitError must be recognized by upstream.IsRateLimited")
	}
	if !IsRateLimited(upstream.CheckStatus("itunes", &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{},
	})) {
		t.Error("an error produced by upstream.CheckStatus must be recognized by metadata.IsRateLimited")
	}
}

func TestProviderRejectedAliasIsRecognizedBySharedClassification(t *testing.T) {
	built := &ProviderRejectedError{Provider: "itunes", Status: http.StatusForbidden}
	if !upstream.IsRejected(built) {
		t.Error("a metadata.ProviderRejectedError must be recognized by upstream.IsRejected")
	}
	if !IsProviderRejected(upstream.CheckStatus("itunes", &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{},
	})) {
		t.Error("an error produced by upstream.CheckStatus must be recognized by metadata.IsProviderRejected")
	}
}

// Classification has to survive wrapping and joining, because callers see the
// failure from several layers deep.
func TestIsRateLimitedThroughWrapping(t *testing.T) {
	base := &RateLimitError{Provider: "itunes", Status: http.StatusTooManyRequests}
	for name, err := range map[string]error{
		"bare":      base,
		"wrapped":   fmt.Errorf("lookup: %w", base),
		"deep":      fmt.Errorf("provider chain: %w", fmt.Errorf("resolve: %w", base)),
		"joined":    errors.Join(ErrNotFound, base),
		"joinedFmt": fmt.Errorf("lookup failed: %w", errors.Join(ErrNotFound, base)),
	} {
		if !IsRateLimited(err) {
			t.Errorf("%s: expected IsRateLimited to be true", name)
		}
	}
	if IsRateLimited(ErrNotFound) {
		t.Error("a plain miss must not be classified as rate limiting")
	}
	if IsRateLimited(fmt.Errorf("iTunes returned HTTP 500")) {
		t.Error("a server error must not be classified as rate limiting")
	}
	if IsRateLimited(nil) {
		t.Error("nil must not be classified as rate limiting")
	}
}

func TestRetryAfterForSurvivesWrapping(t *testing.T) {
	base := &RateLimitError{Provider: "itunes", Status: http.StatusTooManyRequests, RetryAfter: 9000000000}
	if got := RetryAfterFor(fmt.Errorf("wrapped: %w", base)); got != base.RetryAfter {
		t.Errorf("RetryAfterFor = %s, want %s", got, base.RetryAfter)
	}
	if got := RetryAfterFor(ErrNotFound); got != 0 {
		t.Errorf("expected zero cooldown for a miss, got %s", got)
	}
}

func TestRejectionSummaryDelegates(t *testing.T) {
	joined := errors.Join(
		&ProviderRejectedError{Provider: "iTunes", Status: 403},
		&ProviderRejectedError{Provider: "Deezer", Status: 403},
	)
	if got := RejectionSummary(joined); got != "iTunes 403, Deezer 403" {
		t.Errorf("unexpected summary %q", got)
	}
}
