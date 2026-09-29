package jobs

import (
	"context"
	"sync"
	"time"
)

// Gate paces outbound work and reacts to upstream rate limiting.
//
// It layers two controls that jobs need together:
//   - a steady ceiling, so N workers cannot multiply into an upstream flood;
//   - a reactive pause, so a 429 (or a Retry-After hint) stops every worker
//     until the advertised cooldown expires instead of each worker retrying
//     into the same wall.
//
// The base spacing is derived from a per-minute budget. When a provider already
// paces itself, pass a generous budget: the provider's own pacer is the
// binding constraint and the gate simply keeps workers from stacking extra
// requests on top of it.
type Gate struct {
	mu sync.Mutex

	// interval is the minimum spacing between two admitted work units.
	interval time.Duration
	// next is the earliest time the next unit may be admitted.
	next time.Time

	// pausedUntil blocks admission until a provider's cooldown expires.
	pausedUntil time.Time
	// backoff is the current reactive penalty applied after a rate limit.
	backoff time.Duration
	// maxBackoff caps the reactive penalty.
	maxBackoff time.Duration

	// consecutive counts rate limits since the last success, driving backoff.
	consecutive int

	// paused counts how many times the gate had to hold workers back. Jobs
	// report it so a run that spent its time throttled is visible.
	paused int
	// rateLimits counts observed upstream rate-limit rejections.
	rateLimits int
}

// Gate tuning bounds. maxBackoff is deliberately short: the goal is to shed
// load quickly, not to impose a long penalty on a provider that recovers
// immediately.
const (
	defaultMaxBackoff = 30 * time.Second
	// initialBackoff is the first reactive penalty after a rate limit.
	initialBackoff = 2 * time.Second
)

// NewGate returns a Gate admitting at most perMinute work units per minute.
// A perMinute of zero or less means "no additional ceiling": the gate only
// reacts to rate limits, leaving pacing entirely to the provider.
func NewGate(perMinute int) *Gate {
	gate := &Gate{maxBackoff: defaultMaxBackoff}
	if perMinute > 0 {
		gate.interval = time.Minute / time.Duration(perMinute)
		if gate.interval <= 0 {
			gate.interval = time.Millisecond
		}
	}
	return gate
}

// Wait blocks until the caller may issue its next unit of work, honoring both
// the steady ceiling and any active rate-limit pause. It returns ctx.Err() if
// the context ends first.
func (g *Gate) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	now := time.Now()
	// A pending pause is folded into the admission time rather than slept
	// while holding the lock, so the next caller recomputes from the updated
	// pause instead of the stale one.
	if now.Before(g.pausedUntil) {
		g.paused++
		if g.next.Before(g.pausedUntil) {
			g.next = g.pausedUntil
		}
	}
	if g.next.IsZero() || g.next.Before(now) {
		g.next = now
	}
	admit := g.next
	g.next = admit.Add(g.interval)
	g.mu.Unlock()

	wait := time.Until(admit)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Throttle records an upstream rate-limit rejection. Every worker is held until
// max(retryAfter, exponential backoff) has elapsed, so the whole job backs off
// together rather than each lane discovering the limit on its own.
func (g *Gate) Throttle(retryAfter time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rateLimits++
	g.consecutive++
	g.backoff *= 2
	if g.backoff < initialBackoff {
		g.backoff = initialBackoff
	}
	if g.backoff > g.maxBackoff {
		g.backoff = g.maxBackoff
	}
	penalty := g.backoff
	if retryAfter > penalty {
		penalty = retryAfter
	}
	until := time.Now().Add(penalty)
	if until.After(g.pausedUntil) {
		g.pausedUntil = until
	}
	// A pause supersedes any admission already handed out.
	if g.next.Before(until) {
		g.next = until
	}
}

// Succeed records a successful round trip, which decays the reactive backoff so
// a job that resumes after a pause speeds back up to its normal ceiling.
func (g *Gate) Succeed() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.consecutive == 0 {
		return
	}
	g.consecutive--
	if g.consecutive == 0 {
		g.backoff = 0
	}
}

// Stats reports how often the gate throttled and how many upstream rate limits
// it absorbed, for the job's final summary.
func (g *Gate) Stats() (rateLimits int, pauses int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rateLimits, g.paused
}
