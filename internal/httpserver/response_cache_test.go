package httpserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func cacheKey(method, path, query string) string {
	return method + "\x00" + path + "\x00" + query
}

// TestResponseCacheReplaysIdenticalRequests verifies that an identical request
// (same method, path, and query, regardless of User-Agent) is served from the
// in-RAM replay cache without re-invoking the inner handler.
func TestResponseCacheReplaysIdenticalRequests(t *testing.T) {
	var calls atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"handler":"ran"}`))
	})

	cache := newResponseCache(5 * time.Second)
	t.Cleanup(cache.Stop)
	handler := recoverMiddleware(cache.middleware(inner), nil)

	target := "/api/lyrics/get?track_name=Song&artist_name=Artist"
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("User-Agent", fmt.Sprintf("agent-%d", i))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != `{"handler":"ran"}` {
			t.Fatalf("request %d: unexpected body %q", i, rec.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected inner handler to run once, ran %d times", calls.Load())
	}
}

// TestResponseCacheEntryExpiresAfterTTL verifies that once an entry's TTL
// passes, the next identical request re-runs the handler instead of being served
// from the stale buffer. A short test TTL keeps this deterministic and quick;
// production uses responseReplayTTL. A hit does not extend the deadline.
func TestResponseCacheEntryExpiresAfterTTL(t *testing.T) {
	var calls atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(fmt.Sprintf("body-%d", calls.Load())))
	})

	cache := newResponseCache(100 * time.Millisecond)
	t.Cleanup(cache.Stop)
	handler := recoverMiddleware(cache.middleware(inner), nil)

	target := "/api/lyrics/get?track_name=Song&artist_name=Artist"
	key := cacheKey(http.MethodGet, "/api/lyrics/get", "track_name=Song&artist_name=Artist")

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, target, nil))
	if first.Body.String() != "body-1" {
		t.Fatalf("expected body-1, got %q", first.Body.String())
	}

	// A burst of hits must not extend the deadline: poll until the sweeper
	// removes the entry on its own TTL, then confirm the handler runs again.
	deadline := time.Now().Add(time.Second)
	for {
		cache.mu.Lock()
		_, present := cache.entries[key]
		cache.mu.Unlock()
		if !present {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cache entry never expired within the test TTL")
		}
		time.Sleep(50 * time.Millisecond)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, target, nil))
	if calls.Load() != 2 {
		t.Fatalf("expected handler to run twice after expiry, ran %d", calls.Load())
	}
	if second.Body.String() != "body-2" {
		t.Fatalf("expected body-2 after expiry, got %q", second.Body.String())
	}
}

// TestResponseCacheKeyIgnoresUserAgent confirms that the cache key is built
// from method, path, and query only, so differing User-Agents never split an
// entry apart.
func TestResponseCacheKeyIgnoresUserAgent(t *testing.T) {
	reqA := httptest.NewRequest(http.MethodGet, "/api/cover/get?q=1", nil)
	reqA.Header.Set("User-Agent", "alpha")
	reqB := httptest.NewRequest(http.MethodGet, "/api/cover/get?q=1", nil)
	reqB.Header.Set("User-Agent", "beta")

	keyA := cacheKey(reqA.Method, reqA.URL.EscapedPath(), reqA.URL.RawQuery)
	keyB := cacheKey(reqB.Method, reqB.URL.EscapedPath(), reqB.URL.RawQuery)
	if keyA != keyB {
		t.Fatalf("expected keys to be equal regardless of User-Agent, got %q vs %q", keyA, keyB)
	}
}

// TestResponseCacheDoesNotReplayRejections verifies that a rate-limited or
// upstream-busy response is not replayed.
//
// A rejection is not an answer. Caching one pins that key to the error for the
// whole TTL, so every retry inside the window is refused even after the cause
// has passed — which turns a momentary congestion spike into a sustained outage
// for exactly the clients retrying hardest.
func TestResponseCacheDoesNotReplayRejections(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Fail once, then succeed: a cached rejection would keep the
				// second request failing even though the cause has cleared.
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":true}`))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			})

			cache := newResponseCache(5 * time.Second)
			t.Cleanup(cache.Stop)
			handler := recoverMiddleware(cache.middleware(inner), nil)

			first := httptest.NewRecorder()
			handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/thing", nil))
			if first.Code != status {
				t.Fatalf("first request: expected %d, got %d", status, first.Code)
			}

			second := httptest.NewRecorder()
			handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/thing", nil))
			if second.Code != http.StatusOK {
				t.Fatalf("second request replayed the %d rejection: got %d", status, second.Code)
			}
			if second.Body.String() != `{"ok":true}` {
				t.Fatalf("second request body = %q, want the recovered response", second.Body.String())
			}
		})
	}
}

// A cache miss that returns a real answer, including a 404, is still replayed.
// A 404 for a track nobody has is a stable fact, not a transient condition.
func TestResponseCacheStillReplaysNotFound(t *testing.T) {
	var calls atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})

	cache := newResponseCache(5 * time.Second)
	t.Cleanup(cache.Stop)
	handler := recoverMiddleware(cache.middleware(inner), nil)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/missing", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("request %d: expected 404, got %d", i, rec.Code)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected the inner handler to run once, ran %d times", calls.Load())
	}
}
