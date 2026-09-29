package metadata

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestIsRateLimitStatus(t *testing.T) {
	limited := []int{http.StatusTooManyRequests, http.StatusServiceUnavailable}
	for _, status := range limited {
		if !isRateLimitStatus(status) {
			t.Errorf("expected %d to be treated as rate limiting", status)
		}
	}
	other := []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusBadRequest, http.StatusOK}
	for _, status := range other {
		if isRateLimitStatus(status) {
			t.Errorf("expected %d not to be treated as rate limiting", status)
		}
	}
}

func TestIsRateLimitedThroughWrapping(t *testing.T) {
	base := &RateLimitError{Provider: "itunes", Status: http.StatusTooManyRequests}
	// Callers see errors from several layers deep, so the classification must
	// survive wrapping and joining.
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

func TestNewRateLimitErrorParsesRetryAfterSeconds(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	response.Header.Set("Retry-After", "42")

	err := newRateLimitError("itunes", response)
	if !IsRateLimited(err) {
		t.Fatal("expected a rate limit error")
	}
	if err.Status != http.StatusTooManyRequests {
		t.Errorf("unexpected status %d", err.Status)
	}
	if err.RetryAfter != 42*time.Second {
		t.Errorf("expected 42s cooldown, got %s", err.RetryAfter)
	}
	if got := RetryAfterFor(fmt.Errorf("wrapped: %w", err)); got != 42*time.Second {
		t.Errorf("RetryAfterFor = %s, want 42s", got)
	}
}

func TestNewRateLimitErrorParsesRetryAfterHTTPDate(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}
	response.Header.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))

	err := newRateLimitError("deezer", response)
	if err.RetryAfter <= 0 {
		t.Fatalf("expected a positive cooldown from the HTTP-date form, got %s", err.RetryAfter)
	}
	if err.RetryAfter > 30*time.Second {
		t.Errorf("cooldown %s should not exceed the advertised 30s", err.RetryAfter)
	}
}

func TestNewRateLimitErrorIgnoresUnusableRetryAfter(t *testing.T) {
	for _, value := range []string{"", "soon", "-5", "0"} {
		response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
		if value != "" {
			response.Header.Set("Retry-After", value)
		}
		err := newRateLimitError("itunes", response)
		if !IsRateLimited(err) {
			t.Errorf("Retry-After %q: still expected a rate limit error", value)
		}
		if err.RetryAfter != 0 {
			t.Errorf("Retry-After %q: expected no cooldown, got %s", value, err.RetryAfter)
		}
	}
}

func TestRateLimitErrorMessage(t *testing.T) {
	withRetry := (&RateLimitError{Provider: "itunes", Status: 429, RetryAfter: 5 * time.Second}).Error()
	if withRetry == "" {
		t.Error("expected a message")
	}
	without := (&RateLimitError{Provider: "itunes", Status: 429}).Error()
	if without == "" {
		t.Error("expected a message without Retry-After")
	}
	if withRetry == without {
		t.Error("expected the message to mention the advertised cooldown")
	}
}

func TestRetryAfterForNonRateLimitError(t *testing.T) {
	if got := RetryAfterFor(ErrNotFound); got != 0 {
		t.Errorf("expected zero cooldown for a miss, got %s", got)
	}
}

func TestNewRateLimitErrorHandlesNilResponse(t *testing.T) {
	err := newRateLimitError("test", nil)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if err.Provider != "test" {
		t.Errorf("expected provider test, got %s", err.Provider)
	}
	if err.Status != 0 {
		t.Errorf("expected status 0, got %d", err.Status)
	}
}

