package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/config"
)

func rateLimitTestConfig() config.Config {
	return config.Config{
		Port:            "8080",
		RateLimitPerSec: 2,
		RateLimitPerMin: 100,
	}
}

func requestFromIP(t *testing.T, handler http.Handler, method, target, ip string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = ip
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestRateLimitRejectsBurstFromSameIP(t *testing.T) {
	limiter := newRateLimiter(rateLimitTestConfig())
	t.Cleanup(limiter.Stop)
	api := limiter.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rejected := 0
	for i := 0; i < 10; i++ {
		response := requestFromIP(t, api, http.MethodGet, "/api/test", "192.0.2.10:1234")
		if response.Code == http.StatusTooManyRequests {
			rejected++
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("rate-limited response is missing Retry-After")
			}
			var body apiError
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode rate-limit response: %v", err)
			}
			if body != (apiError{Code: http.StatusTooManyRequests, Message: "Rate limit exceeded"}) {
				t.Fatalf("unexpected rate-limit response: %+v", body)
			}
		}
	}
	if rejected == 0 {
		t.Fatal("expected a burst above the per-second limit to be rejected")
	}
}

func TestRateLimitStateIsIndependentPerIP(t *testing.T) {
	limiter := newRateLimiter(rateLimitTestConfig())
	t.Cleanup(limiter.Stop)
	api := limiter.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for i := 0; i < 10; i++ {
		_ = requestFromIP(t, api, http.MethodGet, "/api/test", "192.0.2.20:1234")
	}

	otherIP := requestFromIP(t, api, http.MethodGet, "/api/test", "192.0.2.21:1234")
	if otherIP.Code == http.StatusTooManyRequests {
		t.Fatalf("different IP was affected by another client's limit: got %d", otherIP.Code)
	}
}

func TestRateLimitTrustProxyFlag(t *testing.T) {
	cfg := rateLimitTestConfig()
	cfg.TrustProxy = false
	limiter := newRateLimiter(cfg)
	firstLimiter := limiter
	t.Cleanup(firstLimiter.Stop)
	api := limiter.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for i := 0; i < 10; i++ {
		request := httptest.NewRequest(http.MethodGet, "/api/test", nil)
		request.RemoteAddr = "192.0.2.40:1234"
		request.Header.Set("X-Forwarded-For", "198.51.100.1")
		api.ServeHTTP(httptest.NewRecorder(), request)
	}
	if response := requestFromIP(t, api, http.MethodGet, "/api/test", "192.0.2.40:1234"); response.Code != http.StatusTooManyRequests {
		t.Fatalf("untrusted forwarded address bypassed limiter: got %d", response.Code)
	}

	cfg.TrustProxy = true
	limiter = newRateLimiter(cfg)
	secondLimiter := limiter
	t.Cleanup(secondLimiter.Stop)
	api = limiter.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	first := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	first.RemoteAddr = "192.0.2.40:1234"
	first.Header.Set("X-Forwarded-For", "198.51.100.1")
	api.ServeHTTP(httptest.NewRecorder(), first)
	second := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	second.RemoteAddr = "192.0.2.40:1234"
	second.Header.Set("X-Forwarded-For", "198.51.100.2")
	if response := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		api.ServeHTTP(recorder, second)
		return recorder
	}(); response.Code == http.StatusTooManyRequests {
		t.Fatal("trusted forwarded addresses incorrectly shared a limiter bucket")
	}
}

func TestHealthzIsExemptFromRateLimit(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(rateLimitTestConfig(), metadataDB, lyricsDB)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})

	rejected := 0
	for i := 0; i < 20; i++ {
		// Each request must be distinct. The replay cache sits outside the rate
		// limiter, so a repeated identical query is answered from cache without
		// consuming any budget and would never trip the limiter at all.
		apiResponse := requestFromIP(t, server.Handler, http.MethodGet, fmt.Sprintf("/api/lyrics/search?q=load+%d", i), "192.0.2.30:1234")
		if apiResponse.Code == http.StatusTooManyRequests {
			rejected++
		}

		healthResponse := requestFromIP(t, server.Handler, http.MethodGet, "/api/healthz", "192.0.2.30:1234")
		if healthResponse.Code != http.StatusOK {
			t.Fatalf("health endpoint was throttled: got %d", healthResponse.Code)
		}
	}
	if rejected == 0 {
		t.Fatal("expected API load to trigger rate limiting")
	}
}

// A repeated identical request is answered from the response cache without
// consuming rate-limit budget. That is what makes a burst of retries survivable:
// the server already had the answer, so rejecting the next copy of the same
// question protects nothing.
func TestReplayCacheHitDoesNotConsumeRateLimitBudget(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(rateLimitTestConfig(), metadataDB, lyricsDB)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})

	// Well past the configured per-second limit, all the same request.
	for i := 0; i < 20; i++ {
		response := requestFromIP(t, server.Handler, http.MethodGet, "/api/lyrics/search?q=replayed", "192.0.2.31:1234")
		if response.Code == http.StatusTooManyRequests {
			t.Fatalf("identical request %d was rate limited despite a cache hit", i)
		}
	}
}
