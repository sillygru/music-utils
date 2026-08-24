package reqlog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestQueryStatsEmptyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "request_log.db")

	w, err := Open(dbPath, nil, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	report, err := QueryStats(context.Background(), dbPath, StatsOptions{})
	if err != nil {
		t.Fatalf("query stats: %v", err)
	}
	if report.TotalRequests != 0 {
		t.Fatalf("expected 0 total requests, got %d", report.TotalRequests)
	}
}

func TestQueryStatsWithRecords(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "request_log.db")

	w, err := Open(dbPath, nil, testLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	// Insert multiple records spanning different endpoints, outcomes, statuses, and times
	records := []Record{
		{
			TS:        now.Add(-2 * time.Hour),
			Method:    "GET",
			Endpoint:  "/api/lyrics/get",
			Status:    200,
			Outcome:   "local_hit",
			CacheMs:   4,
			UserAgent: "test-client",
		},
		{
			TS:         now.Add(-1 * time.Hour),
			Method:     "GET",
			Endpoint:   "/api/metadata/get",
			Status:     200,
			Outcome:    "provider_fallback_hit",
			CacheMs:    5,
			UpstreamMs: 150,
			UserAgent:  "curl",
		},
		{
			TS:        now.Add(-30 * time.Minute),
			Method:    "GET",
			Endpoint:  "/api/cover/get",
			Status:    404,
			Outcome:   "miss",
			CacheMs:   2,
			UserAgent: "curl",
		},
		{
			TS:        now.Add(-10 * time.Minute),
			Method:    "GET",
			Endpoint:  "/api/lyrics/get",
			Status:    429,
			Outcome:   "rate_limited",
			CacheMs:   1,
			UserAgent: "python-requests",
		},
		{
			TS:        now.Add(-2 * 24 * time.Hour),
			Method:    "GET",
			Endpoint:  "/api/lyrics/get",
			Status:    200,
			Outcome:   "local_hit",
			CacheMs:   3,
			UserAgent: "test-client",
		},
	}

	for _, rec := range records {
		w.Log(rec)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	report, err := QueryStats(context.Background(), dbPath, StatsOptions{DailyDays: 7, TopLimit: 5})
	if err != nil {
		t.Fatalf("query stats: %v", err)
	}

	if report.TotalRequests != 5 {
		t.Fatalf("expected 5 total requests, got %d", report.TotalRequests)
	}

	if report.Status2xxCount != 3 {
		t.Errorf("expected 3 2xx requests, got %d", report.Status2xxCount)
	}
	if report.Status4xxCount != 2 {
		t.Errorf("expected 2 4xx requests, got %d", report.Status4xxCount)
	}
	if report.Status429Count != 1 {
		t.Errorf("expected 1 429 request, got %d", report.Status429Count)
	}
	if report.LocalHitCount != 2 {
		t.Errorf("expected 2 local hits, got %d", report.LocalHitCount)
	}
	if report.FallbackHitCount != 1 {
		t.Errorf("expected 1 fallback hit, got %d", report.FallbackHitCount)
	}
	if report.MissCount != 1 {
		t.Errorf("expected 1 miss, got %d", report.MissCount)
	}

	if len(report.Windows) != 4 {
		t.Fatalf("expected 4 windows, got %d", len(report.Windows))
	}
	// 24h window should have 4 requests (one is 2 days old)
	if report.Windows[0].Requests != 4 {
		t.Errorf("expected 4 requests in 24h window, got %d", report.Windows[0].Requests)
	}
	// 7d window should have 5 requests
	if report.Windows[1].Requests != 5 {
		t.Errorf("expected 5 requests in 7d window, got %d", report.Windows[1].Requests)
	}

	if len(report.Endpoints) == 0 {
		t.Fatal("expected top endpoints to be populated")
	}
	if report.Endpoints[0].Endpoint != "/api/lyrics/get" || report.Endpoints[0].Requests != 3 {
		t.Errorf("unexpected top endpoint: %+v", report.Endpoints[0])
	}

	if len(report.UserAgents) == 0 {
		t.Fatal("expected top user agents to be populated")
	}
}

func TestQueryStatsNonExistentPath(t *testing.T) {
	_, err := QueryStats(context.Background(), "/non/existent/path.db", StatsOptions{})
	if err == nil {
		t.Fatal("expected error on non-existent path")
	}
}
