// Package upstream classifies provider HTTP failures into the two kinds a
// long-running caller has to tell apart: the caller is over budget, or the
// provider is refusing this caller.
//
// The distinction matters because the two demand opposite reactions. A rate
// limit is about the moment, so the caller should back off and try again. A
// refusal is about the caller itself, so retrying a different track gets the
// same answer and only spends the run's budget. Without this classification a
// provider that has blocked the host is indistinguishable from a bad moment, and
// the batch jobs either hammer it or give up on the whole run.
package upstream

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
// formatted error so long-running callers (the batch jobs) can slow down, back
// off, and resume instead of guessing from error text.
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

// RejectedError reports that a provider refused the request outright rather
// than throttling it. iTunes answers HTTP 403 to a blocked network regardless
// of User-Agent, and every request thereafter fails the same way, so it is a
// property of the caller rather than of any one track.
type RejectedError struct {
	// Provider is the provider name that refused the request.
	Provider string
	// Status is the HTTP status returned, normally 401, 403, or 451.
	Status int
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d (request rejected; this host or User-Agent is refused)", e.Provider, e.Status)
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

// IsRejected reports whether err, or any error it wraps, is a provider refusing
// the request rather than rate limiting or failing to answer.
//
// A long-running caller uses it to stop early: retrying a different track
// against a provider that rejects the caller cannot succeed, so a run that
// discovers this should say so once rather than spend its whole budget on
// lookups that were never going to work.
func IsRejected(err error) bool {
	var rejected *RejectedError
	return errors.As(err, &rejected)
}

// RejectionSummary names every provider that refused the request, as a short
// "iTunes 403, Deezer 403" list. Callers surface it so an operator can see which
// upstream is the problem without reading a joined error chain.
func RejectionSummary(err error) string {
	var found []*RejectedError
	collectRejections(err, &found)
	if len(found) == 0 {
		return "no provider detail"
	}
	parts := make([]string, 0, len(found))
	seen := make(map[string]struct{}, len(found))
	for _, rejection := range found {
		label := fmt.Sprintf("%s %d", rejection.Provider, rejection.Status)
		if _, dup := seen[label]; dup {
			continue
		}
		seen[label] = struct{}{}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

// collectRejections walks a possibly-joined error tree gathering every provider
// refusal. errors.As stops at the first match, which would report one broken
// provider as the only one when the whole chain is down.
func collectRejections(err error, out *[]*RejectedError) {
	if err == nil {
		return
	}
	var rejection *RejectedError
	if errors.As(err, &rejection) {
		*out = append(*out, rejection)
	}
	switch joined := err.(type) {
	case interface{ Unwrap() []error }:
		for _, inner := range joined.Unwrap() {
			collectRejections(inner, out)
		}
	case interface{ Unwrap() error }:
		collectRejections(joined.Unwrap(), out)
	}
}

// CheckStatus turns a non-successful HTTP response into the error a caller
// should act on, and returns nil for a 2xx response.
//
// Every provider client funnels its status handling through this one function,
// which is what makes the classification consistent across upstreams. Handing
// back a typed error rather than a formatted string is the point: a caller
// decides whether to retry by asking IsRateLimited or IsRejected, never by
// matching on text that a provider is free to reword.
//
// A 404 is reported as rejected=false and rate-limited=false, so callers keep
// classifying it as a genuine per-track miss via their own ErrNotFound sentinel;
// pass the sentinel through rather than relying on the status here.
func CheckStatus(provider string, response *http.Response) error {
	if response == nil {
		return nil
	}
	status := response.StatusCode
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		return nil
	}
	switch {
	case IsRateLimitStatus(status):
		return newRateLimitError(provider, response)
	case IsRejectedStatus(status):
		return &RejectedError{Provider: provider, Status: status}
	default:
		return fmt.Errorf("%s returned HTTP %d", provider, status)
	}
}

// IsRateLimitStatus reports whether an HTTP status means the caller, not the
// song, is over budget. 503 is included because providers use it for capacity
// backpressure; both warrant backing off rather than being cached as a miss.
func IsRateLimitStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// IsRejectedStatus reports whether a status means the provider refused this
// caller outright. These are not retriable: the same request from the same
// network gets the same answer, so backing off and requeueing only wastes the
// run's budget. 404 is excluded because it is a legitimate per-track miss.
func IsRejectedStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusUnavailableForLegalReasons:
		return true
	}
	return false
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
