package httpserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFallbackBudgetLimitsUniqueMissesPerIP(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	cfg := fallbackConfig(upstream.URL + "/api")
	cfg.FallbackPerMin = 2
	cfg.FallbackMaxQueue = 10
	metadataDB, lyricsDB := testHTTPDatabases(t)
	seedHTTPTrack(t, metadataDB, lyricsDB)
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	first := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Ghost+One&artist_name=Artist")
	if first.Code != http.StatusNotFound {
		t.Fatalf("expected first miss 404, got %d: %s", first.Code, first.Body.String())
	}
	second := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Ghost+Two&artist_name=Artist")
	if second.Code != http.StatusNotFound {
		t.Fatalf("expected second miss 404, got %d: %s", second.Code, second.Body.String())
	}
	third := performRequest(t, server.Handler, "/api/lyrics/get?track_name=Ghost+Three&artist_name=Artist")
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("expected third miss to be budget-limited to 429, got %d: %s", third.Code, third.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("expected two upstream calls before budget exhaustion, got %d", calls.Load())
	}

	// Cache hits must not consume the fallback budget.
	local := performRequest(t, server.Handler, "/api/lyrics/get?track_name=example+song&artist_name=example+artist")
	if local.Code != http.StatusOK {
		t.Fatalf("expected local hit to bypass the budget, got %d: %s", local.Code, local.Body.String())
	}
}

func TestUpstreamGateFailsFastWhenQueueSaturated(t *testing.T) {
	hold := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hold) }) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		http.NotFound(w, r)
	}))
	// LIFO ordering: release runs before upstream.Close, so a failing test can
	// never deadlock the httptest server shutdown on the held handler.
	defer upstream.Close()
	defer release()

	cfg := fallbackConfig(upstream.URL + "/api")
	cfg.FallbackPerMin = 100
	cfg.FallbackMaxQueue = 1
	cfg.LRCLIBTimeoutMS = 5000
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(cfg, metadataDB, lyricsDB)
	cleanupHTTPServer(t, server)

	const requests = 4
	results := make(chan int, requests)
	for i := 0; i < requests; i++ {
		go func(n int) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/lyrics/get?track_name=Ghost+%d&artist_name=Artist", n), nil)
			server.Handler.ServeHTTP(recorder, request)
			results <- recorder.Code
		}(i)
	}

	// The single queue slot is held by one request (blocked upstream until
	// hold closes), so the first three arrivals must all be fail-fast 503s.
	for i := 0; i < requests-1; i++ {
		select {
		case code := <-results:
			if code != http.StatusServiceUnavailable {
				t.Fatalf("expected fail-fast 503, got %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for fail-fast 503 responses")
		}
	}

	// Release the held request; it completes as an upstream 404.
	release()
	select {
	case code := <-results:
		if code != http.StatusNotFound {
			t.Fatalf("expected the held request to return 404, got %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the held request")
	}
}
