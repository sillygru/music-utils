package httpserver

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// responseReplayTTL is how long a cached response is replayed before it is
// dropped. A request that hits the cache does NOT extend this window: each
// entry lives exactly TTL past the moment it was first produced, so repeated
// identical requests keep returning the same output until the timer expires.
const responseReplayTTL = 5 * time.Second

// replaySweepInterval is how often the background sweeper removes expired
// entries. Kept small relative to the TTL so each entry is cleaned up on (or
// just after) its own 5-second deadline.
const replaySweepInterval = 250 * time.Millisecond

// cachedResponse is the captured status, headers, and body of one response.
type cachedResponse struct {
	status int
	header http.Header
	body   []byte
	// expiry is the absolute deadline; reading an entry never moves it.
	expiry time.Time
}

// responseCache replays identical requests without re-running the database or
// upstream work. Entries are keyed by method, path, and query string, so a
// distinct User-Agent does not create a separate entry.
type responseCache struct {
	mu         sync.Mutex
	entries    map[string]cachedResponse
	ttl        time.Duration
	sweepEvery time.Duration
	stopCh     chan struct{}
	doneCh     chan struct{}
}

func newResponseCache(ttl time.Duration) *responseCache {
	sweep := ttl / 10
	if sweep <= 0 {
		sweep = replaySweepInterval
	}
	c := &responseCache{
		entries:    make(map[string]cachedResponse),
		ttl:        ttl,
		sweepEvery: sweep,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	go c.run()
	return c
}

// middleware wraps a handler so a cache hit short-circuits the whole inner
// stack (database lookups included) and replays the stored response verbatim.
// The per-entry timer is not refreshed by a hit.
func (c *responseCache) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + "\x00" + r.URL.EscapedPath() + "\x00" + r.URL.RawQuery
		if cached, ok := c.get(key, time.Now()); ok {
			setOutcome(r, "replay_cache_hit")
			for name, values := range cached.header {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(cached.status)
			_, _ = w.Write(cached.body)
			return
		}

		rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		c.set(key, cachedResponse{
			status: rec.statusCode(),
			header: w.Header().Clone(),
			body:   rec.body.Bytes(),
		})
	})
}

func (c *responseCache) set(key string, res cachedResponse) {
	c.mu.Lock()
	res.expiry = time.Now().Add(c.ttl)
	c.entries[key] = res
	c.mu.Unlock()
}

func (c *responseCache) get(key string, now time.Time) (cachedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !now.Before(entry.expiry) {
		if ok {
			delete(c.entries, key)
		}
		return cachedResponse{}, false
	}
	return entry, true
}

func (c *responseCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if !now.Before(entry.expiry) {
			delete(c.entries, key)
		}
	}
}

func (c *responseCache) run() {
	defer close(c.doneCh)
	ticker := time.NewTicker(c.sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case now := <-ticker.C:
			c.sweep(now)
		}
	}
}

// Stop shuts down the background sweeper and waits for it to finish.
func (c *responseCache) Stop() {
	select {
	case <-c.stopCh:
		return
	default:
	}
	close(c.stopCh)
	<-c.doneCh
}

// recordingWriter captures the status and body a handler wrote so they can be
// replayed later. It forwards all writes to the underlying ResponseWriter so
// the client still receives the response on the first (cache-missing) request.
type recordingWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
	body   bytes.Buffer
}

func (w *recordingWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.body.Write(body)
	return w.ResponseWriter.Write(body)
}

func (w *recordingWriter) statusCode() int {
	return w.status
}
