package metadata

import (
	"time"

	"github.com/sillygru/music-utils/internal/upstream"
)

// The provider failure classification lives in internal/upstream so the lyrics
// providers classify their responses exactly the way the metadata providers do.
// A batch job walks both provider families in one run, and it can only pause on
// a throttle or stop on a refusal if the two agree on what those mean.
//
// These are aliases rather than distinct types on purpose. errors.As matches on
// the concrete type, so a defined type would stop a metadata.RateLimitError
// produced here from being recognized by a lyrics provider's error, and the
// caller's classification would silently depend on which package the failure
// happened to come from.

// RateLimitError reports that a provider refused a request because the caller
// exceeded that provider's upstream budget.
type RateLimitError = upstream.RateLimitError

// ProviderRejectedError reports that a provider refused the request outright
// rather than throttling it, which is a property of the caller rather than of
// any one track. It is named for the metadata providers' original spelling;
// upstream.RejectedError is the same type under the name the shared package
// uses for all provider families.
type ProviderRejectedError = upstream.RejectedError

// IsRateLimited reports whether err, or any error it wraps, is an upstream
// rate-limit or capacity rejection. Callers use it to decide when to back off
// rather than to treat the failure as a permanent lookup miss.
func IsRateLimited(err error) bool { return upstream.IsRateLimited(err) }

// RetryAfterFor returns the server-advertised cooldown carried by err, or zero
// when err is not a rate-limit rejection or advertised no cooldown.
func RetryAfterFor(err error) time.Duration { return upstream.RetryAfterFor(err) }

// IsProviderRejected reports whether err, or any error it wraps, is a provider
// refusing the request rather than rate limiting or failing to answer.
func IsProviderRejected(err error) bool { return upstream.IsRejected(err) }

// RejectionSummary names every provider that refused the request, as a short
// "iTunes 403, Deezer 403" list.
func RejectionSummary(err error) string { return upstream.RejectionSummary(err) }
