package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCORSHeadersOnAPIResponses(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(rateLimitTestConfig(), metadataDB, lyricsDB)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})

	response := requestFromIP(t, server.Handler, http.MethodGet, "/api/lyrics/search?q=example", "192.0.2.50:1234")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("missing Access-Control-Allow-Origin on API response, got %q", got)
	}
}

func TestCORSPreflightIsAnsweredWithoutRateLimiting(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	server := NewWithConfig(rateLimitTestConfig(), metadataDB, lyricsDB)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})

	// A burst of preflights from one IP must all succeed (204) and never be
	// rate limited: preflights are browser-generated overhead, not API calls.
	for i := 0; i < 15; i++ {
		request := httptest.NewRequest(http.MethodOptions, "/api/lyrics/get", nil)
		request.RemoteAddr = "192.0.2.51:1234"
		request.Header.Set("Origin", "https://example.com")
		request.Header.Set("Access-Control-Request-Method", "GET")
		recorder := httptest.NewRecorder()
		server.Handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNoContent {
			t.Fatalf("preflight %d: expected 204, got %d", i, recorder.Code)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("preflight %d: missing allow-origin, got %q", i, got)
		}
		if got := recorder.Header().Get("Access-Control-Allow-Methods"); got != "GET, OPTIONS" {
			t.Fatalf("preflight %d: unexpected allow-methods %q", i, got)
		}
		if recorder.Header().Get("Access-Control-Allow-Headers") == "" {
			t.Fatalf("preflight %d: missing allow-headers", i)
		}
		if recorder.Header().Get("Access-Control-Max-Age") == "" {
			t.Fatalf("preflight %d: missing max-age", i)
		}
	}
}

func TestCORSHeadersOnRateLimitedResponse(t *testing.T) {
	limiter := newRateLimiter(rateLimitTestConfig())
	t.Cleanup(limiter.Stop)
	api := corsMiddleware(limiter.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	var rejected *httptest.ResponseRecorder
	for i := 0; i < 10; i++ {
		response := requestFromIP(t, api, http.MethodGet, "/api/test", "192.0.2.52:1234")
		if response.Code == http.StatusTooManyRequests {
			rejected = response
			break
		}
	}
	if rejected == nil {
		t.Fatal("expected a rate-limited response")
	}
	// Browsers must still be able to read the 429 body, so the CORS header
	// has to be present on rejected responses too.
	if got := rejected.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("rate-limited response is missing CORS header, got %q", got)
	}
}
