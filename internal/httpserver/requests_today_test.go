package httpserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestRequestsTodayEndpointReportsLiveCount(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	cfg := rateLimitTestConfig()
	cfg.RequestLogEnabled = true
	cfg.RequestLogDBPath = filepath.Join(t.TempDir(), "request_log.db")
	cfg.RequestLogRetentionDays = 30
	cfg.RequestsTodayEnabled = true
	server, requestLogs := NewWithLogger(cfg, metadataDB, lyricsDB, nil, nil)
	if requestLogs == nil {
		t.Fatal("expected a request log writer when enabled")
	}

	// Real API requests must show up in the count; health and version probes
	// are exempt.
	if response := performRequest(t, server.Handler, "/api/lyrics/search?q=example&limit=5"); response.Code != http.StatusOK {
		t.Fatalf("search failed: %d", response.Code)
	}
	if response := performRequest(t, server.Handler, "/api/healthz"); response.Code != http.StatusOK {
		t.Fatalf("healthz failed: %d", response.Code)
	}
	if response := performRequest(t, server.Handler, "/api/version"); response.Code != http.StatusOK {
		t.Fatalf("version failed: %d", response.Code)
	}

	// The stats request itself must not be counted, and neither are the probes,
	// so the number stays 1.
	response := performRequest(t, server.Handler, requestsTodayPath)
	if response.Code != http.StatusOK {
		t.Fatalf("stats failed: %d", response.Code)
	}
	var body struct {
		RequestsToday int64 `json:"requestsToday"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode stats response: %v", err)
	}
	if body.RequestsToday != 1 {
		t.Fatalf("expected 1 request in the last 24 hours, got %d", body.RequestsToday)
	}

	// A further stats poll still must not inflate the count.
	response = performRequest(t, server.Handler, requestsTodayPath)
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode stats response: %v", err)
	}
	if body.RequestsToday != 1 {
		t.Fatalf("expected stats polls to be excluded, got %d", body.RequestsToday)
	}

	if err := requestLogs.Close(); err != nil {
		t.Fatalf("close request log: %v", err)
	}
}

func TestRequestsTodayEndpointDisabledIsNotFound(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	cfg := rateLimitTestConfig() // RequestsTodayEnabled is false on a zero Config
	server := NewWithConfig(cfg, metadataDB, lyricsDB)

	if response := performRequest(t, server.Handler, requestsTodayPath); response.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when disabled, got %d", response.Code)
	}
}

func TestRequestsTodayEndpointReturnsZeroWithoutRequestLog(t *testing.T) {
	metadataDB, lyricsDB := testHTTPDatabases(t)
	cfg := rateLimitTestConfig()
	cfg.RequestLogEnabled = false
	cfg.RequestLogDBPath = filepath.Join(t.TempDir(), "should-not-exist.db")
	cfg.RequestsTodayEnabled = true
	server := NewWithConfig(cfg, metadataDB, lyricsDB)

	response := performRequest(t, server.Handler, requestsTodayPath)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	var body struct {
		RequestsToday int64 `json:"requestsToday"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode stats response: %v", err)
	}
	if body.RequestsToday != 0 {
		t.Fatalf("expected 0 with request logging off, got %d", body.RequestsToday)
	}
	if _, err := os.Stat(cfg.RequestLogDBPath); !os.IsNotExist(err) {
		t.Fatalf("expected no request log database when disabled, stat err=%v", err)
	}
}
