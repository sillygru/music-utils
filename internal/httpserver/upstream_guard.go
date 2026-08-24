package httpserver

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/sillygru/music-utils/internal/config"
)

const (
	// upstreamQueueWait is how long a cache-missing request waits for a slot
	// in the upstream queue before failing fast with 503 instead of queueing
	// behind a pile of other misses. It is also the default when a
	// directly-constructed Config in tests leaves the wait at zero.
	upstreamQueueWait = 10 * time.Second
	// upstreamQueueRetryAfter is the Retry-After value advertised with 503s.
	upstreamQueueRetryAfter = upstreamQueueWait / time.Second
)

// upstreamGate bounds how many cache-missing requests may be inside the
// upstream provider layer at once. When saturated, the provider queue is
// backed up; new misses fail fast so a single client cannot make everyone
// else wait in line.
type upstreamGate struct {
	sem chan struct{}
}

func newUpstreamGate(capacity int) *upstreamGate {
	if capacity < 1 {
		capacity = 1
	}
	return &upstreamGate{sem: make(chan struct{}, capacity)}
}

// acquire reserves one slot, waiting up to wait. The returned release must be
// called exactly once. ok is false when the queue is saturated or the request
// context is canceled.
func (g *upstreamGate) acquire(ctx context.Context, wait time.Duration) (release func(), ok bool) {
	if wait <= 0 {
		wait = upstreamQueueWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case g.sem <- struct{}{}:
		return func() { <-g.sem }, true
	case <-timer.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

// fallbackGuard combines the per-IP fallback budget and the shared upstream
// queue gate that protect cache-missing requests: the budget caps how much
// upstream spend a single client can trigger per minute, and the gate bounds
// how many misses may wait inside the provider layer at once.
type fallbackGuard struct {
	budget     *rateLimiter
	gate       *upstreamGate
	trustProxy bool
	queueWait  time.Duration
}

func newFallbackGuard(cfg config.Config) *fallbackGuard {
	budgetCfg := cfg
	// The budget only needs the per-minute window; disable the token bucket.
	budgetCfg.RateLimitPerSec = 1 << 30
	// A zero value (directly-constructed Config in tests) is normalized by the
	// rate limiter to the default per-minute window, mirroring the main
	// limiter's behavior.
	budgetCfg.RateLimitPerMin = cfg.FallbackPerMin
	queueWait := time.Duration(cfg.FallbackQueueWaitMS) * time.Millisecond
	if queueWait <= 0 {
		queueWait = upstreamQueueWait
	}
	return &fallbackGuard{
		budget:     newRateLimiter(budgetCfg),
		gate:       newUpstreamGate(cfg.FallbackMaxQueue),
		trustProxy: cfg.TrustProxy,
		queueWait:  queueWait,
	}
}

// acquire reserves one upstream fallback attempt without writing an HTTP
// response. It lets request coalescers reserve budget only for the leader of
// an identical lookup; waiters share that leader's result and consume nothing.
func (g *fallbackGuard) acquire(r *http.Request) (release func(), status, retryAfter int, proceed bool) {
	return g.acquireFor(r.Context(), clientIP(r, g.trustProxy))
}

func (g *fallbackGuard) acquireFor(ctx context.Context, key string) (release func(), status, retryAfter int, proceed bool) {
	if allowed, retry := g.budget.allow(key); !allowed {
		return nil, http.StatusTooManyRequests, retry, false
	}
	release, ok := g.gate.acquire(ctx, g.queueWait)
	if !ok {
		return nil, http.StatusServiceUnavailable, int(upstreamQueueRetryAfter), false
	}
	return release, 0, 0, true
}

// enter gates one upstream fallback attempt. When proceed is false the guard
// has already written the 429 (per-IP budget) or 503 (queue saturated)
// response and the handler must return without touching upstream.
func (g *fallbackGuard) enter(r *http.Request, w http.ResponseWriter) (release func(), proceed bool) {
	release, status, retryAfter, ok := g.acquire(r)
	if ok {
		return release, true
	}
	if status == http.StatusTooManyRequests {
		setOutcome(r, "rate_limited")
		writeRateLimitResponse(w, retryAfter)
		return nil, false
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	setOutcome(r, "upstream_busy")
	writeJSON(w, http.StatusServiceUnavailable, apiError{
		Code:    http.StatusServiceUnavailable,
		Message: "Upstream busy, try again shortly",
	})
	return nil, false
}

// Stop shuts down the budget limiter's cleanup goroutine.
func (g *fallbackGuard) Stop() { g.budget.Stop() }
