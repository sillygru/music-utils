// Package pacer paces outbound requests so a shared upstream provider budget
// is never exceeded, regardless of how many concurrent client requests are
// driving lookups.
package pacer

import (
	"context"
	"sync"
	"time"
)

// Pacer ensures callers wait until a fixed interval has elapsed since the
// previous request. The first call is never delayed; every later call is
// spaced at least interval apart, so the process issues at most one request
// per interval to the paced endpoint.
type Pacer struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
}

// New returns a Pacer that spaces requests at least interval apart.
func New(interval time.Duration) *Pacer {
	return &Pacer{interval: interval}
}

// Wait blocks until the caller may issue its next request, honoring ctx. The
// mutex is held across the wait so concurrent callers serialize: each caller
// computes its wait from the most recently recorded request time, which keeps
// spacing strict exactly when requests arrive in bursts.
func (p *Pacer) Wait(ctx context.Context) error {
	if p == nil || p.interval <= 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last.IsZero() {
		p.last = time.Now()
		return nil
	}
	wait := time.Until(p.last.Add(p.interval))
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.last = time.Now()
	return nil
}
