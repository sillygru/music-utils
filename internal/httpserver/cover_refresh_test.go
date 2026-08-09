package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
)

func refreshJobConfig() config.Config {
	return config.Config{
		CoverRefreshEnabled:    true,
		CoverRefreshAfterDays:  30,
		CoverRefreshStartHour:  0,
		CoverRefreshEndHour:    23,
		CoverRefreshMaxRows:    100,
		CoverRefreshMaxRecheck: 10,
		CoverUserAgent:         "music-utils-test",
	}
}

func TestCoverRefreshWindow(t *testing.T) {
	job := &coverRefreshJob{startHour: 2, endHour: 5}
	inside := time.Date(2026, 8, 8, 3, 30, 0, 0, time.UTC)
	if !job.inWindow(inside) {
		t.Fatal("expected 03:30 to be inside the 02:00-05:00 window")
	}
	before := time.Date(2026, 8, 8, 1, 59, 0, 0, time.UTC)
	if job.inWindow(before) {
		t.Fatal("expected 01:59 to be outside the window")
	}
	atEnd := time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)
	if job.inWindow(atEnd) {
		t.Fatal("expected the end hour to be exclusive")
	}

	overnight := &coverRefreshJob{startHour: 22, endHour: 2}
	if !overnight.inWindow(time.Date(2026, 8, 8, 23, 0, 0, 0, time.UTC)) {
		t.Fatal("expected 23:00 to be inside the wrapping 22:00-02:00 window")
	}
	if !overnight.inWindow(time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)) {
		t.Fatal("expected 01:00 to be inside the wrapping window")
	}
	if overnight.inWindow(time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("expected noon to be outside the wrapping window")
	}
}

func TestCoverRefreshSweepRefreshesStaleRows(t *testing.T) {
	artwork := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alive.jpg":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer artwork.Close()

	// The recheck path filters provider results for plausibility, so the stub's
	// re-resolved result must carry the requested artist name.
	stub := &coverStubProvider{name: "itunes", result: &cover.Result{URL: "http://fresh/cover.jpg", Source: "itunes", ArtistName: "The Beatles"}}
	coverDB := testCoverDB(t)
	ctx := context.Background()

	// Two stale positive rows: one alive, one dead.
	for _, row := range []struct {
		artist string
		url    string
	}{
		{"Radiohead", artwork.URL + "/alive.jpg"},
		{"The Beatles", artwork.URL + "/dead.jpg"},
	} {
		if err := db.UpsertCoverArt(ctx, coverDB, db.CoverArtist, row.artist, "", row.url, "itunes"); err != nil {
			t.Fatalf("seed %s: %v", row.artist, err)
		}
	}
	if _, err := coverDB.ExecContext(ctx, `UPDATE cover_urls SET checked_at = datetime('now', '-40 days')`); err != nil {
		t.Fatalf("age cover rows: %v", err)
	}

	// One fresh row that must not be touched. checked_at is read through
	// COALESCE like production code, because the driver returns bare DATETIME
	// columns in a different format than raw text.
	if err := db.UpsertCoverArt(ctx, coverDB, db.CoverArtist, "Fresh Artist", "", artwork.URL+"/alive.jpg", "itunes"); err != nil {
		t.Fatalf("seed fresh row: %v", err)
	}
	var freshChecked string
	if err := coverDB.QueryRowContext(ctx, `SELECT COALESCE(checked_at,'') FROM cover_urls WHERE artist_name_lower = 'fresh artist'`).Scan(&freshChecked); err != nil {
		t.Fatalf("read fresh checked_at: %v", err)
	}

	// Record the aged alive row's own timestamp before the sweep so the bump
	// assertion is not sensitive to second-granularity collisions.
	var radioheadBefore string
	if err := coverDB.QueryRowContext(ctx, `SELECT COALESCE(checked_at,'') FROM cover_urls WHERE artist_name_lower = 'radiohead'`).Scan(&radioheadBefore); err != nil {
		t.Fatalf("read radiohead checked_at: %v", err)
	}

	job := newCoverRefreshJob(refreshJobConfig(), coverDB, cover.NewResolver(stub), slog.Default())
	// Sweep at a fixed in-window hour so the test is deterministic regardless
	// of the wall clock (the configured window is 00:00–23:00 local).
	job.sweep(ctx, time.Date(2026, 8, 8, 3, 30, 0, 0, time.UTC))
	job.Stop()

	var deadURL string
	if err := coverDB.QueryRowContext(ctx, `SELECT COALESCE(cover_url,'') FROM cover_urls WHERE artist_name_lower = 'the beatles'`).Scan(&deadURL); err != nil {
		t.Fatalf("read refreshed row: %v", err)
	}
	if deadURL != "http://fresh/cover.jpg" {
		t.Fatalf("expected dead URL to be re-resolved, got %q", deadURL)
	}
	if stub.calls != 1 {
		t.Fatalf("expected one re-resolution for the dead row, got %d", stub.calls)
	}

	var aliveURL, aliveChecked string
	if err := coverDB.QueryRowContext(ctx, `SELECT cover_url, COALESCE(checked_at,'') FROM cover_urls WHERE artist_name_lower = 'radiohead'`).Scan(&aliveURL, &aliveChecked); err != nil {
		t.Fatalf("read alive row: %v", err)
	}
	if aliveURL != artwork.URL+"/alive.jpg" {
		t.Fatalf("expected the alive URL to be kept, got %q", aliveURL)
	}
	if !timeAfter(aliveChecked, radioheadBefore) {
		t.Fatalf("expected the alive row's checked_at to be bumped, got %q vs %q", aliveChecked, radioheadBefore)
	}

	var freshAgain string
	if err := coverDB.QueryRowContext(ctx, `SELECT COALESCE(checked_at,'') FROM cover_urls WHERE artist_name_lower = 'fresh artist'`).Scan(&freshAgain); err != nil {
		t.Fatalf("read fresh row again: %v", err)
	}
	if freshAgain != freshChecked {
		t.Fatalf("expected the fresh row to be untouched, got %q != %q", freshAgain, freshChecked)
	}
}

func TestCoverRefreshPromotesLiveVariant(t *testing.T) {
	artwork := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alive-variant.jpg":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer artwork.Close()

	// No provider should ever be consulted: the live alternate is cached.
	stub := &coverStubProvider{name: "itunes", result: &cover.Result{URL: "http://fresh/cover.jpg", Source: "itunes", ArtistName: "Radiohead"}}
	coverDB := testCoverDB(t)
	ctx := context.Background()

	// The winner URL is dead; the alternate is alive.
	if err := db.UpsertCoverArtVariants(ctx, coverDB, db.CoverArtist, "Radiohead", "", []db.CoverVariant{
		{URL: artwork.URL + "/dead.jpg", Source: "lastfm"},
		{URL: artwork.URL + "/alive-variant.jpg", Source: "itunes"},
	}); err != nil {
		t.Fatalf("seed variants: %v", err)
	}
	if _, err := coverDB.ExecContext(ctx, `UPDATE cover_urls SET checked_at = datetime('now', '-40 days')`); err != nil {
		t.Fatalf("age cover rows: %v", err)
	}
	var before string
	if err := coverDB.QueryRowContext(ctx, `SELECT COALESCE(checked_at,'') FROM cover_urls WHERE artist_name_lower = 'radiohead'`).Scan(&before); err != nil {
		t.Fatalf("read pre-sweep checked_at: %v", err)
	}

	job := newCoverRefreshJob(refreshJobConfig(), coverDB, cover.NewResolver(stub), slog.Default())
	// Sweep at a fixed in-window hour so the test is deterministic regardless
	// of the wall clock (the configured window is 00:00–23:00 local).
	job.sweep(ctx, time.Date(2026, 8, 8, 3, 30, 0, 0, time.UTC))
	job.Stop()

	var winner, checked string
	if err := coverDB.QueryRowContext(ctx, `SELECT cover_url, COALESCE(checked_at,'') FROM cover_urls WHERE artist_name_lower = 'radiohead'`).Scan(&winner, &checked); err != nil {
		t.Fatalf("read promoted row: %v", err)
	}
	if winner != artwork.URL+"/alive-variant.jpg" {
		t.Fatalf("expected the live variant to be promoted to winner, got %q", winner)
	}
	if !timeAfter(checked, before) {
		t.Fatalf("expected checked_at to be bumped after promotion, got %q vs %q", checked, before)
	}
	if stub.calls != 0 {
		t.Fatalf("expected no upstream call when a live variant exists, got %d", stub.calls)
	}

	// Ranks swap: the promoted URL is now rank 0, the dead former winner 1.
	var coverURLID int64
	if err := coverDB.QueryRowContext(ctx, `SELECT id FROM cover_urls WHERE artist_name_lower = 'radiohead'`).Scan(&coverURLID); err != nil {
		t.Fatalf("read cover row id: %v", err)
	}
	variants, err := db.FindCoverVariants(ctx, coverDB, coverURLID)
	if err != nil {
		t.Fatalf("read variants after promotion: %v", err)
	}
	if len(variants) != 2 || variants[0].Rank != 0 || variants[0].URL != artwork.URL+"/alive-variant.jpg" || variants[1].Rank != 1 || variants[1].URL != artwork.URL+"/dead.jpg" {
		t.Fatalf("unexpected variant order after promotion: %+v", variants)
	}
}

// timeAfter reports whether the timestamps in checkedAt format satisfy
// later > earlier, avoiding string comparisons at second granularity.
func timeAfter(later, earlier string) bool {
	a, errA := time.Parse(lastFMTimeFormat, later)
	b, errB := time.Parse(lastFMTimeFormat, earlier)
	if errA != nil || errB != nil {
		return later > earlier
	}
	return a.After(b)
}

func TestCoverRefreshSweepDisabledSkips(t *testing.T) {
	cfg := refreshJobConfig()
	cfg.CoverRefreshEnabled = false
	database, err := db.Open(":memory:", db.Config{MmapSize: 1024 * 1024, CacheSizeKB: -2, MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.MigrateCover(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	job := newCoverRefreshJob(cfg, database, cover.NewResolver(&coverStubProvider{name: "lastfm"}), slog.Default())
	job.sweep(context.Background(), time.Now())
	job.Stop()
	// No panic, no query against the empty database is the assertion here.
	var count int
	if err := database.QueryRowContext(context.Background(), `SELECT count(*) FROM cover_urls`).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no rows, got %d", count)
	}
}
