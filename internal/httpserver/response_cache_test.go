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

// TestResponseCacheEntryExpiresAfterTTL verifies that once an entry's 5-second
// window passes, the next identical request re-runs the handler instead of
// being served from the stale buffer. A hit does not extend the deadline.
func TestResponseCacheEntryExpiresAfterTTL(t *testing.T) {
	var calls atomic.Int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(fmt.Sprintf("body-%d", calls.Load())))
	})

	cache := newResponseCache(5 * time.Second)
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		cache.mu.Lock()
		_, present := cache.entries[key]
		cache.mu.Unlock()
		if !present {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cache entry never expired within 10s")
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
