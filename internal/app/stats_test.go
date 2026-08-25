package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
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

func TestRunStatsWithCachedContent(t *testing.T) {
	dir := t.TempDir()
	reqLogPath := filepath.Join(dir, "request_log.db")
	metadataPath := filepath.Join(dir, "metadata.db")
	lyricsPath := filepath.Join(dir, "lyrics.db")
	coverPath := filepath.Join(dir, "cover.db")

	// Create request log
	w, err := reqlog.Open(reqLogPath, nil, nil)
	if err != nil {
		t.Fatalf("open reqlog: %v", err)
	}
	w.Log(reqlog.Record{
		TS:        time.Now(),
		Method:    "GET",
		Endpoint:  "/api/metadata/get",
		Status:    200,
		Outcome:   "local_hit",
		CacheMs:   5,
		UserAgent: "test",
	})
	if err := w.Close(); err != nil {
		t.Fatalf("close reqlog: %v", err)
	}

	// Create cache databases
	dbCfg := db.Config{MmapSize: 512 * 1024 * 1024, CacheSizeKB: -64000, MaxOpenConns: 1}
	metadataDB, err := db.Open(metadataPath, dbCfg)
	if err != nil {
		t.Fatalf("open metadata: %v", err)
	}
	defer metadataDB.Close()
	lyricsDB, err := db.Open(lyricsPath, dbCfg)
	if err != nil {
		t.Fatalf("open lyrics: %v", err)
	}
	defer lyricsDB.Close()
	coverDB, err := db.Open(coverPath, dbCfg)
	if err != nil {
		t.Fatalf("open cover: %v", err)
	}
	defer coverDB.Close()

	ctx := context.Background()
	if err := db.MigrateMetadata(ctx, metadataDB); err != nil {
		t.Fatalf("migrate metadata: %v", err)
	}
	if err := db.MigrateLyrics(ctx, lyricsDB); err != nil {
		t.Fatalf("migrate lyrics: %v", err)
	}
	if err := db.MigrateCover(ctx, coverDB); err != nil {
		t.Fatalf("migrate cover: %v", err)
	}

	// Seed tracks
	tracks := []db.Track{
		{Name: "Song One", ArtistName: "Artist A", AlbumName: "Album A", Duration: 200, CoverURL: "http://cover/1"},
		{Name: "song one", ArtistName: "Artist B", AlbumName: "Album B", Duration: 210, CoverURL: "http://cover/2"},
		{Name: "Song Two", ArtistName: "Artist A", AlbumName: "Album A", Duration: 180},
	}
	for i, track := range tracks {
		if i < 2 {
			_, _, err = db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, db.Lyrics{PlainLyrics: "lyrics text"})
		} else {
			_, err = db.UpsertTrackMetadata(ctx, metadataDB, track)
		}
		if err != nil {
			t.Fatalf("insert track %d: %v", i, err)
		}
	}
	if err := db.UpsertCoverArt(ctx, coverDB, db.CoverAlbum, "Artist A", "Album A", "http://cover/album", "deezer"); err != nil {
		t.Fatalf("insert album cover: %v", err)
	}
	if err := db.UpsertCoverArt(ctx, coverDB, db.CoverArtist, "Artist A", "", "http://cover/artist", "deezer"); err != nil {
		t.Fatalf("insert artist cover: %v", err)
	}

	var out, errOut bytes.Buffer
	code := RunStatsTo(&out, &errOut, []string{
		"-db", reqLogPath,
		"-metadata", metadataPath,
		"-lyrics", lyricsPath,
		"-cover", coverPath,
	})
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d (err: %s)", code, errOut.String())
	}

	output := out.String()
	for _, expected := range []string{
		"CACHED CONTENT",
		"Unique Songs:",
		"Total Cached:",
		"Song Metadata:",
		"Song Lyrics:",
		"Song Covers:",
		"Album Covers:",
		"Artist Covers:",
		"Total Covers:",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("missing %q in output:\n%s", expected, output)
		}
	}
}
