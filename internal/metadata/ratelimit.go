package metadata

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitError reports that a provider refused a request because the caller
// exceeded that provider's upstream budget. It is returned instead of a plain
// formatted error so long-running callers (the jobs backfill) can slow down,
// back off, and resume instead of guessing from error text.
type RateLimitError struct {
	// Provider is the provider name that rejected the request.
	Provider string
	// Status is the HTTP status returned, normally 429 or 503.
	Status int
	// RetryAfter is the server-advertised cooldown. Zero when the response
	// carried no usable Retry-After header.
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s returned HTTP %d, retry after %s", e.Provider, e.Status, e.RetryAfter)
	}
	return fmt.Sprintf("%s returned HTTP %d (rate limited)", e.Provider, e.Status)
}

// IsRateLimited reports whether err, or any error it wraps, is an upstream
// rate-limit or capacity rejection. Callers use it to decide when to back off
// rather than to treat the failure as a permanent lookup miss.
func IsRateLimited(err error) bool {
	var rateLimited *RateLimitError
	return errors.As(err, &rateLimited)
}

// RetryAfterFor returns the server-advertised cooldown carried by err, or zero
// when err is not a rate-limit rejection or advertised no cooldown.
func RetryAfterFor(err error) time.Duration {
	var rateLimited *RateLimitError
	if errors.As(err, &rateLimited) {
		return rateLimited.RetryAfter
	}
	return 0
}

// isRateLimitStatus reports whether an HTTP status means the caller, not the
// song, is the problem. 503 is included because providers use it for capacity
// backpressure; both warrant backing off rather than being cached as a miss.
func isRateLimitStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// newRateLimitError builds a RateLimitError from a provider response, reading
// Retry-After when present. The header is honored in both its delta-seconds and
// HTTP-date forms; anything unparseable is ignored rather than guessed at.
func newRateLimitError(provider string, response *http.Response) *RateLimitError {
	if response == nil {
		return &RateLimitError{Provider: provider}
	}
	err := &RateLimitError{Provider: provider, Status: response.StatusCode}
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if value == "" {
		return err
	}
	if seconds, convErr := strconv.Atoi(value); convErr == nil {
		if seconds > 0 {
			err.RetryAfter = time.Duration(seconds) * time.Second
		}
		return err
	}
	if when, parseErr := http.ParseTime(value); parseErr == nil {
		if wait := time.Until(when); wait > 0 {
			err.RetryAfter = wait
		}
	}
	return err
}
