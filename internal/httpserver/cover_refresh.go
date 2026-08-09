package httpserver

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/pacer"
)

const (
	coverRefreshTickInterval    = time.Hour
	coverRefreshCheckInterval   = 200 * time.Millisecond
	coverRefreshValidateTimeout = 10 * time.Second
)

// coverRefreshRow is one candidate row selected for revalidation.
type coverRefreshRow struct {
	id         int64
	entityType db.CoverEntity
	artist     string
	album      string
	url        string
}

// coverRefreshJob proactively revalidates cached positive cover rows that have
// aged past COVER_REFRESH_AFTER_DAYS, running only inside the configured
// low-activity window. Rows whose artwork URL still responds are refreshed in
// place (checked_at bumped); rows whose winner is dead first promote a
// still-live cached variant (no upstream spend) and are only re-resolved
// through the provider chain when every variant is dead. The job draws from
// the providers' shared pacing, so it can never exceed upstream rate limits,
// and it yields to live traffic by running only in the configured window with
// a bounded batch per tick.
type coverRefreshJob struct {
	database     *sql.DB
	resolver     *cover.Resolver
	logger       *slog.Logger
	enabled      bool
	startHour    int
	endHour      int
	refreshAfter time.Duration
	maxPerRun    int
	maxRecheck   int
	userAgent    string
	client       *http.Client
	checkPace    *pacer.Pacer
	stop         context.CancelFunc
	stopped      chan struct{}
	stopOnce     sync.Once
}

func newCoverRefreshJob(cfg config.Config, database *sql.DB, resolver *cover.Resolver, logger *slog.Logger) *coverRefreshJob {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &coverRefreshJob{
		database:     database,
		resolver:     resolver,
		logger:       logger,
		enabled:      cfg.CoverRefreshEnabled,
		startHour:    cfg.CoverRefreshStartHour,
		endHour:      cfg.CoverRefreshEndHour,
		refreshAfter: time.Duration(cfg.CoverRefreshAfterDays) * 24 * time.Hour,
		maxPerRun:    cfg.CoverRefreshMaxRows,
		maxRecheck:   cfg.CoverRefreshMaxRecheck,
		userAgent:    cfg.CoverUserAgent,
		client:       &http.Client{Timeout: coverRefreshValidateTimeout},
		checkPace:    pacer.New(coverRefreshCheckInterval),
		stop:         cancel,
		stopped:      make(chan struct{}),
	}
	go job.run(ctx)
	return job
}

// run ticks hourly and performs one bounded sweep per tick inside the
// configured low-activity window.
func (j *coverRefreshJob) run(ctx context.Context) {
	ticker := time.NewTicker(coverRefreshTickInterval)
	defer ticker.Stop()
	defer close(j.stopped)
	for {
		select {
		case <-ticker.C:
			j.sweep(ctx, time.Now())
		case <-ctx.Done():
			return
		}
	}
}

// Stop shuts the job down, waiting for the run loop to exit.
func (j *coverRefreshJob) Stop() {
	j.stopOnce.Do(func() {
		j.stop()
		<-j.stopped
	})
}

// inWindow reports whether now falls inside the configured refresh window.
// A start hour greater than the end hour wraps across midnight.
func (j *coverRefreshJob) inWindow(now time.Time) bool {
	hour := now.Hour()
	if j.startHour <= j.endHour {
		return hour >= j.startHour && hour < j.endHour
	}
	return hour >= j.startHour || hour < j.endHour
}

// sweep revalidates one bounded batch of stale positive cover rows: cheapest
// first (a range GET against the artwork CDN), and only rows whose URL is
// dead consume the expensive provider-chain re-resolution budget.
func (j *coverRefreshJob) sweep(ctx context.Context, now time.Time) {
	if !j.enabled || j.database == nil || j.resolver == nil {
		return
	}
	if !j.inWindow(now) {
		return
	}
	cutoff := now.UTC().Add(-j.refreshAfter).Format(lastFMTimeFormat)
	rows, err := j.database.QueryContext(ctx, `SELECT id, entity_type, COALESCE(artist_name_lower,''), COALESCE(album_name_lower,''), COALESCE(cover_url,'') FROM cover_urls WHERE cover_url IS NOT NULL AND cover_url <> '' AND checked_at < ? ORDER BY checked_at ASC LIMIT ?`, cutoff, j.maxPerRun)
	if err != nil {
		j.logger.Error("cover refresh query failed", "error", err)
		return
	}
	// Collect the batch and close the cursor before writing: SQLite pool
	// configurations with a single connection would otherwise deadlock on the
	// per-row updates while the cursor still holds the connection.
	var batch []coverRefreshRow
	for rows.Next() {
		var row coverRefreshRow
		if err := rows.Scan(&row.id, &row.entityType, &row.artist, &row.album, &row.url); err != nil {
			rows.Close()
			j.logger.Error("cover refresh scan failed", "error", err)
			return
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		j.logger.Error("cover refresh iteration failed", "error", err)
		return
	}
	rows.Close()

	processed, dead, rechecked := 0, 0, 0
	for _, row := range batch {
		alive, isDead := j.validateURL(ctx, row.url)
		switch {
		case alive:
			if _, err := j.database.ExecContext(ctx, "UPDATE cover_urls SET checked_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ?", row.id); err != nil {
				j.logger.Error("cover refresh bump failed", "error", err)
			}
		case isDead:
			if j.promoteAliveVariant(ctx, row) {
				// A live alternate was already cached; no upstream spend.
				break
			}
			if rechecked >= j.maxRecheck {
				j.logger.Info("cover refresh sweep truncated", "processed", processed, "dead", dead, "rechecked", rechecked)
				return // leave the rest of the batch for the next run
			}
			rechecked++
			dead++
			j.recheck(ctx, row.entityType, row.artist, row.album)
		default:
			// Inconclusive (timeout, 403, 5xx): leave checked_at untouched so
			// the row is retried later rather than treated as dead.
		}
		processed++
	}
	if processed > 0 {
		j.logger.Info("cover refresh sweep complete", "processed", processed, "dead", dead, "rechecked", rechecked)
	}
}

// validateURL reports whether artworkURL still serves artwork. A range GET is
// used because some CDNs answer HEAD inconsistently; reading only the first
// bytes keeps the check cheap. 404/410 mean the artwork is gone; other
// non-2xx responses and network errors are inconclusive, not dead.
func (j *coverRefreshJob) validateURL(ctx context.Context, artworkURL string) (alive, dead bool) {
	if err := j.checkPace.Wait(ctx); err != nil {
		return false, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artworkURL, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", j.userAgent)
	req.Close = true
	resp, err := j.client.Do(req)
	if err != nil {
		return false, false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return false, true
	case http.StatusOK, http.StatusPartialContent, http.StatusMovedPermanently, http.StatusFound,
		http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusNotModified:
		return true, false
	default:
		return false, false
	}
}

// promoteAliveVariant swaps the first still-live cached alternate into the
// winner slot when the current winner URL is dead, without contacting any
// provider. It reports whether a live alternate was found and promoted.
func (j *coverRefreshJob) promoteAliveVariant(ctx context.Context, row coverRefreshRow) bool {
	variants, err := db.FindCoverVariants(ctx, j.database, row.id)
	if err != nil {
		j.logger.Error("cover refresh variant query failed", "error", err)
		return false
	}
	for _, variant := range variants {
		if variant.Rank == 0 || variant.URL == "" || variant.URL == row.url {
			continue
		}
		alive, isDead := j.validateURL(ctx, variant.URL)
		if isDead {
			// Drop dead alternates so they are not re-validated on every sweep.
			if _, err := j.database.ExecContext(ctx, `DELETE FROM cover_url_variants WHERE cover_url_id = ? AND url = ?`, row.id, variant.URL); err != nil {
				j.logger.Error("cover refresh variant cleanup failed", "error", err)
			}
			continue
		}
		if !alive {
			// Inconclusive (timeout, 403, 5xx): leave it for the next sweep.
			continue
		}
		if err := db.PromoteCoverVariant(ctx, j.database, row.id, variant.URL, variant.Source, variant.Rank); err != nil {
			j.logger.Error("cover refresh variant promote failed", "error", err)
			return false
		}
		return true
	}
	return false
}

// recheck re-resolves artwork for a dead URL through the provider chain and
// stores the fresh URL set (every plausible provider result, not just the
// winner), or records a negative miss when the artwork is gone.
func (j *coverRefreshJob) recheck(ctx context.Context, entity db.CoverEntity, artist, album string) {
	input := cover.Input{ArtistName: artist, AlbumName: album}
	results, err := j.resolver.Search(ctx, toKind(entity), input, 50)
	if err == nil {
		results = filterCoverResults(toKind(entity), input, results)
	}
	if err != nil || len(results) == 0 {
		if upsertErr := db.UpsertCoverArt(ctx, j.database, entity, artist, album, "", ""); upsertErr != nil {
			j.logger.Error("cover refresh negative upsert failed", "error", upsertErr)
		}
		return
	}
	variants := make([]db.CoverVariant, 0, len(results))
	for _, result := range results {
		variants = append(variants, db.CoverVariant{URL: result.URL, Source: result.Source})
	}
	if upsertErr := db.UpsertCoverArtVariants(ctx, j.database, entity, artist, album, variants); upsertErr != nil {
		j.logger.Error("cover refresh upsert failed", "error", upsertErr)
	}
}
