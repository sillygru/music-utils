package upstream

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
		if !IsRateLimitStatus(status) {
			t.Errorf("expected %d to be treated as rate limiting", status)
		}
	}
	other := []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusBadRequest, http.StatusOK}
	for _, status := range other {
		if IsRateLimitStatus(status) {
			t.Errorf("expected %d not to be treated as rate limiting", status)
		}
	}
}

func TestIsRejectedStatus(t *testing.T) {
	rejected := []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusUnavailableForLegalReasons}
	for _, status := range rejected {
		if !IsRejectedStatus(status) {
			t.Errorf("expected %d to be treated as a refusal", status)
		}
	}
	// 404 is the one status that must stay a per-track miss: a provider saying
	// "no such song" is an answer, not a refusal of the caller.
	other := []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusOK}
	for _, status := range other {
		if IsRejectedStatus(status) {
			t.Errorf("expected %d not to be treated as a refusal", status)
		}
	}
}

func TestCheckStatusPassesSuccessfulResponses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent} {
		if err := CheckStatus("itunes", &http.Response{StatusCode: status}); err != nil {
			t.Errorf("status %d: expected no error, got %v", status, err)
		}
	}
	if err := CheckStatus("itunes", nil); err != nil {
		t.Errorf("nil response: expected no error, got %v", err)
	}
}

func TestCheckStatusClassifiesRateLimits(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		err := CheckStatus("lrclib", &http.Response{StatusCode: status, Header: http.Header{}})
		if !IsRateLimited(err) {
			t.Errorf("status %d: expected a rate limit error, got %v", status, err)
		}
		if IsRejected(err) {
			t.Errorf("status %d: a rate limit must not also read as a refusal", status)
		}
	}
}

func TestCheckStatusClassifiesRefusals(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusUnavailableForLegalReasons} {
		err := CheckStatus("lrclib", &http.Response{StatusCode: status, Header: http.Header{}})
		if !IsRejected(err) {
			t.Errorf("status %d: expected a refusal, got %v", status, err)
		}
		if IsRateLimited(err) {
			t.Errorf("status %d: a refusal must not also read as a rate limit", status)
		}
	}
}

// A status that is neither a throttle nor a refusal must stay an ordinary
// error. Callers fall back to their own per-track miss sentinel for these, and
// wrongly typing one would persist a transient failure as a permanent negative.
func TestCheckStatusLeavesOtherStatusesUntyped(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusBadRequest, http.StatusInternalServerError, http.StatusBadGateway} {
		err := CheckStatus("lrclib", &http.Response{StatusCode: status, Header: http.Header{}})
		if err == nil {
			t.Errorf("status %d: expected an error", status)
			continue
		}
		if IsRateLimited(err) {
			t.Errorf("status %d: must not be classified as rate limiting", status)
		}
		if IsRejected(err) {
			t.Errorf("status %d: must not be classified as a refusal", status)
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
		"joined":    errors.Join(errors.New("no such song"), base),
		"joinedFmt": fmt.Errorf("lookup failed: %w", errors.Join(errors.New("no such song"), base)),
	} {
		if !IsRateLimited(err) {
			t.Errorf("%s: expected IsRateLimited to be true", name)
		}
	}
	if IsRateLimited(errors.New("no such song")) {
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
	if got := RetryAfterFor(errors.New("no such song")); got != 0 {
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

func TestRejectionSummaryNamesEveryProvider(t *testing.T) {
	joined := errors.Join(
		&RejectedError{Provider: "iTunes", Status: 403},
		&RejectedError{Provider: "Deezer", Status: 403},
		// A repeat of the same refusal must not be listed twice, or the summary
		// overstates how many upstreams are refusing the caller.
		&RejectedError{Provider: "iTunes", Status: 403},
	)
	summary := RejectionSummary(joined)
	if summary != "iTunes 403, Deezer 403" {
		t.Errorf("unexpected summary %q", summary)
	}
	if got := RejectionSummary(errors.New("boom")); got != "no provider detail" {
		t.Errorf("unexpected summary %q", got)
	}
}
