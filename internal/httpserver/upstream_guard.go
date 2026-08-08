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
	// behind a pile of other misses.
	upstreamQueueWait = 2 * time.Second
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

// acquire reserves one slot, waiting up to upstreamQueueWait. The returned
// release must be called exactly once. ok is false when the queue is
// saturated or the request context is canceled.
func (g *upstreamGate) acquire(ctx context.Context) (release func(), ok bool) {
	timer := time.NewTimer(upstreamQueueWait)
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
}

func newFallbackGuard(cfg config.Config) *fallbackGuard {
	budgetCfg := cfg
	// The budget only needs the per-minute window; disable the token bucket.
	budgetCfg.RateLimitPerSec = 1 << 30
	// A zero value (directly-constructed Config in tests) is normalized by the
	// rate limiter to the default per-minute window, mirroring the main
	// limiter's behavior.
	budgetCfg.RateLimitPerMin = cfg.FallbackPerMin
	return &fallbackGuard{
		budget:     newRateLimiter(budgetCfg),
		gate:       newUpstreamGate(cfg.FallbackMaxQueue),
		trustProxy: cfg.TrustProxy,
	}
}

// enter gates one upstream fallback attempt. When proceed is false the guard
// has already written the 429 (per-IP budget) or 503 (queue saturated)
// response and the handler must return without touching upstream.
//
// Note: the budget is consumed before the provider call. For metadata and
// cover lookups the resolver memoizes misses in memory, so a repeated miss
// consumes budget without spending upstream; this over-count is intentional
// and harmless at the default budget of 10/min.
func (g *fallbackGuard) enter(r *http.Request, w http.ResponseWriter) (release func(), proceed bool) {
	ip := clientIP(r, g.trustProxy)
	if allowed, retryAfter := g.budget.allow(ip); !allowed {
		setOutcome(r, "rate_limited")
		writeRateLimitResponse(w, retryAfter)
		return nil, false
	}
	release, ok := g.gate.acquire(r.Context())
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(upstreamQueueRetryAfter)))
		setOutcome(r, "upstream_busy")
		writeJSON(w, http.StatusServiceUnavailable, apiError{
			Code:    http.StatusServiceUnavailable,
			Message: "Upstream busy, try again shortly",
		})
		return nil, false
	}
	return release, true
}

// Stop shuts down the budget limiter's cleanup goroutine.
func (g *fallbackGuard) Stop() { g.budget.Stop() }
