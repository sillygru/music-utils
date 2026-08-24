package app

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/reqlog"
)

func TestRunStatsMissingDB(t *testing.T) {
	var out, errOut bytes.Buffer
	code := RunStatsTo(&out, &errOut, []string{"-db", "/non/existent/request_log.db"})
	if code != 1 {
		t.Fatalf("expected exit code 1 for missing db, got %d", code)
	}
	if !strings.Contains(errOut.String(), "cannot open database") {
		t.Fatalf("expected error message in stderr, got: %s", errOut.String())
	}
}

func TestRunStatsEmptyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "request_log.db")

	w, err := reqlog.Open(dbPath, nil, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out, errOut bytes.Buffer
	code := RunStatsTo(&out, &errOut, []string{"-db", dbPath})
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d (err: %s)", code, errOut.String())
	}
	output := out.String()
	if !strings.Contains(output, "MUSIC-UTILS REQUEST STATS") {
		t.Fatalf("expected header in output, got: %s", output)
	}
	if !strings.Contains(output, "No request records logged yet") {
		t.Fatalf("expected empty status message, got: %s", output)
	}
}

func TestRunStatsWithData(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "request_log.db")

	w, err := reqlog.Open(dbPath, nil, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	w.Log(reqlog.Record{
		TS:        now.Add(-10 * time.Minute),
		Method:    "GET",
		Endpoint:  "/api/lyrics/get",
		Status:    200,
		Outcome:   "local_hit",
		CacheMs:   5,
		UserAgent: "curl",
	})
	w.Log(reqlog.Record{
		TS:         now.Add(-5 * time.Minute),
		Method:     "GET",
		Endpoint:   "/api/metadata/get",
		Status:     200,
		Outcome:    "provider_fallback_hit",
		CacheMs:    8,
		UpstreamMs: 120,
		UserAgent:  "browser-chromium",
	})
	w.Log(reqlog.Record{
		TS:        now.Add(-1 * time.Minute),
		Method:    "GET",
		Endpoint:  "/api/cover/get",
		Status:    404,
		Outcome:   "miss",
		CacheMs:   2,
		UserAgent: "python-requests",
	})
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out, errOut bytes.Buffer
	code := RunStatsTo(&out, &errOut, []string{"-db", dbPath, "-days", "7", "-top", "5"})
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d (err: %s)", code, errOut.String())
	}

	output := out.String()
	if !strings.Contains(output, "MUSIC-UTILS REQUEST STATS") {
		t.Errorf("missing header in output:\n%s", output)
	}
	if !strings.Contains(output, "ACTIVITY WINDOWS") {
		t.Errorf("missing activity windows in output:\n%s", output)
	}
	if !strings.Contains(output, "TOP ENDPOINTS") {
		t.Errorf("missing top endpoints in output:\n%s", output)
	}
	if !strings.Contains(output, "OUTCOMES & TOP USER AGENTS") {
		t.Errorf("missing outcomes section in output:\n%s", output)
	}
	if !strings.Contains(output, "/api/lyrics/get") {
		t.Errorf("missing /api/lyrics/get endpoint in output:\n%s", output)
	}
}
