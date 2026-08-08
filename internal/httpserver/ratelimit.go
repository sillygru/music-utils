package httpserver

import (
	"context"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"golang.org/x/time/rate"
)

const (
	limiterCleanupInterval = 5 * time.Minute
	limiterStaleAfter      = 10 * time.Minute
)

type limiterEntry struct {
	mu       sync.Mutex
	perSec   *rate.Limiter
	requests []time.Time
	lastSeen time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	config  config.Config
	stop    context.CancelFunc
	stopped chan struct{}
	stopOne sync.Once
}

func newRateLimiter(cfg config.Config) *rateLimiter {
	cfg = normalizedRateLimitConfig(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	limiter := &rateLimiter{
		entries: make(map[string]*limiterEntry),
		config:  cfg,
		stop:    cancel,
		stopped: make(chan struct{}),
	}
	go limiter.cleanup(ctx)
	return limiter
}

func (l *rateLimiter) cleanup(ctx context.Context) {
	ticker := time.NewTicker(limiterCleanupInterval)
	defer ticker.Stop()
	defer close(l.stopped)

	for {
		select {
		case <-ticker.C:
			l.evictStale(time.Now())
		case <-ctx.Done():
			return
		}
	}
}

func (l *rateLimiter) evictStale(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for ip, entry := range l.entries {
		entry.mu.Lock()
		stale := now.Sub(entry.lastSeen) > limiterStaleAfter
		entry.mu.Unlock()
		if stale {
			delete(l.entries, ip)
		}
	}
}

func (l *rateLimiter) Stop() {
	l.stopOne.Do(func() {
		l.stop()
		<-l.stopped
	})
}

func (l *rateLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		ip := clientIP(r, l.config.TrustProxy)
		if allowed, retryAfter := l.allow(ip); !allowed {
			setOutcome(r, "rate_limited")
			writeRateLimitResponse(w, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow records one request for ip under the limiter's per-second and
// per-minute limits and reports whether it may proceed. retryAfter is only
// meaningful when allowed is false. It is safe to call concurrently and is
// reused by the fallback budget limiter.
func (l *rateLimiter) allow(ip string) (allowed bool, retryAfter int) {
	entry := l.entryFor(ip)
	now := time.Now()

	entry.lastSeen = now
	entry.requests = recentRequests(entry.requests, now.Add(-time.Minute))

	if len(entry.requests) >= l.config.RateLimitPerMin {
		retryAfter := retryAfterWindow(entry.requests[0], now)
		entry.mu.Unlock()
		return false, retryAfter
	}
	if !entry.perSec.Allow() {
		entry.mu.Unlock()
		return false, 1
	}

	entry.requests = append(entry.requests, now)
	entry.mu.Unlock()
	return true, 0
}

// entryFor returns an entry with its mutex held. The caller must unlock it.
func (l *rateLimiter) entryFor(ip string) *limiterEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.entries[ip]
	if !ok {
		entry = &limiterEntry{
			perSec: rate.NewLimiter(rate.Limit(l.config.RateLimitPerSec), l.config.RateLimitPerSec),
		}
		l.entries[ip] = entry
	}
	// Lock the entry before releasing the map lock so cleanup cannot evict this
	// entry between lookup and use, which could split one IP across buckets.
	entry.mu.Lock()
	return entry
}

func recentRequests(requests []time.Time, cutoff time.Time) []time.Time {
	firstRecent := 0
	for firstRecent < len(requests) && !requests[firstRecent].After(cutoff) {
		firstRecent++
	}
	return requests[firstRecent:]
}

func retryAfterWindow(firstRequest, now time.Time) int {
	seconds := math.Ceil(firstRequest.Add(time.Minute).Sub(now).Seconds())
	if seconds < 1 {
		return 1
	}
	return int(seconds)
}

func normalizedRateLimitConfig(cfg config.Config) config.Config {
	if cfg.RateLimitPerSec < 1 {
		cfg.RateLimitPerSec = 10
	}
	if cfg.RateLimitPerMin < 1 {
		cfg.RateLimitPerMin = 180
	}
	return cfg
}

func writeRateLimitResponse(w http.ResponseWriter, retryAfter int) {
	if retryAfter < 1 {
		retryAfter = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeJSON(w, http.StatusTooManyRequests, apiError{
		Code:    http.StatusTooManyRequests,
		Message: "Rate limit exceeded",
	})
}

func clientIP(r *http.Request, trustProxy bool) string {
	address := r.RemoteAddr
	if trustProxy {
		if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
			address = strings.TrimSpace(strings.Split(forwarded, ",")[0])
		}
	}

	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return strings.Trim(strings.TrimSpace(address), "[]")
}
