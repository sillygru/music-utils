package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
)

// prefetchBudgetKey is the fixed rate-limiter key for background prefetch
// spend. It is distinct from any client IP, so background work never consumes
// a client's per-IP fallback budget and a client's misses never starve the
// background prefetcher.
const prefetchBudgetKey = "__prefetch__"

// prefetchJob identifies one song whose related content should be cached.
type prefetchJob struct {
	trackName  string
	artistName string
	albumName  string
	duration   float64
}

// prefetcher quietly resolves related content (lyrics, album and artist
// covers) for songs that were successfully looked up, so later requests
// become local cache hits. Every target first checks the local caches and only
// spends upstream budget when something is genuinely missing. All upstream
// calls flow through the providers' shared pacers, so the prefetcher can never
// exceed an upstream rate limit; a dedicated per-minute budget caps how much
// background spend it may trigger regardless of client traffic.
type prefetcher struct {
	metadataDB   *sql.DB
	lyricsDB     *sql.DB
	coverDB      *sql.DB
	coverRes     *cover.Resolver
	lrclibClient *lrclib.Client
	lyricsMisses *lyricsMissCache
	logger       *slog.Logger

	enabled          bool
	fetchLyrics      bool
	fetchAlbumCover  bool
	fetchArtistCover bool

	budget    *rateLimiter
	workers   int
	queue     chan prefetchJob
	inFlight  map[string]struct{}
	inFlightM sync.Mutex

	stop     context.CancelFunc
	stopped  chan struct{}
	stopOnce sync.Once
}

func newPrefetcher(cfg config.Config, metadataDB, lyricsDB, coverDB *sql.DB, coverRes *cover.Resolver, lrclibClient *lrclib.Client, lyricsMisses *lyricsMissCache, logger *slog.Logger) *prefetcher {
	if !cfg.PrefetchEnabled {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	// The budget only needs the per-minute window; disable the token bucket so
	// bursts are throttled by the per-minute count rather than a tiny bucket.
	budgetCfg := cfg
	budgetCfg.RateLimitPerSec = 1 << 30
	budgetCfg.RateLimitPerMin = cfg.PrefetchPerMin
	if budgetCfg.RateLimitPerMin < 1 {
		budgetCfg.RateLimitPerMin = 1
	}
	workers := cfg.PrefetchConcurrency
	if workers < 1 {
		workers = 1
	}
	queueSize := cfg.PrefetchQueueSize
	if queueSize < 1 {
		queueSize = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &prefetcher{
		metadataDB:       metadataDB,
		lyricsDB:         lyricsDB,
		coverDB:          coverDB,
		coverRes:         coverRes,
		lrclibClient:     lrclibClient,
		lyricsMisses:     lyricsMisses,
		logger:           logger,
		enabled:          cfg.PrefetchEnabled,
		fetchLyrics:      cfg.PrefetchLyrics,
		fetchAlbumCover:  cfg.PrefetchAlbumCover,
		fetchArtistCover: cfg.PrefetchArtistCover,
		budget:           newRateLimiter(budgetCfg),
		workers:          workers,
		queue:            make(chan prefetchJob, queueSize),
		inFlight:         make(map[string]struct{}),
		stop:             cancel,
		stopped:          make(chan struct{}),
	}
	go p.run(ctx)
	return p
}

// run drains the queue with up to workers concurrent jobs until the context
// is canceled.
func (p *prefetcher) run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-p.queue:
					p.process(ctx, job)
				}
			}
		}()
	}
	wg.Wait()
	close(p.stopped)
}

// Stop shuts the prefetcher down, waiting for workers to exit and releasing
// the budget limiter's cleanup goroutine.
func (p *prefetcher) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.stop()
		<-p.stopped
		p.budget.Stop()
	})
}

// Enqueue schedules background prefetch for a successfully-looked-up song. It
// is fire-and-forget: it never blocks the request and never fails it. Duplicate
// jobs for the same identity already queued or in flight are dropped, as are
// jobs submitted when the queue is full.
func (p *prefetcher) Enqueue(trackName, artistName, albumName string, duration float64) {
	if p == nil || !p.enabled {
		return
	}
	trackName = strings.TrimSpace(trackName)
	artistName = strings.TrimSpace(artistName)
	albumName = strings.TrimSpace(albumName)
	if trackName == "" && artistName == "" && albumName == "" {
		return
	}
	job := prefetchJob{trackName: trackName, artistName: artistName, albumName: albumName, duration: duration}
	key := prefetchJobKey(job)
	p.inFlightM.Lock()
	if _, ok := p.inFlight[key]; ok {
		p.inFlightM.Unlock()
		return
	}
	p.inFlight[key] = struct{}{}
	p.inFlightM.Unlock()
	select {
	case p.queue <- job:
	default:
		p.inFlightM.Lock()
		delete(p.inFlight, key)
		p.inFlightM.Unlock()
		p.logger.Debug("prefetch queue full, dropping job", "track", trackName, "artist", artistName)
	}
}

func (p *prefetcher) process(ctx context.Context, job prefetchJob) {
	defer p.finish(job)
	if p.fetchLyrics {
		p.prefetchLyrics(ctx, job)
	}
	if p.fetchAlbumCover {
		p.prefetchAlbumCover(ctx, job)
	}
	if p.fetchArtistCover {
		p.prefetchArtistCover(ctx, job)
	}
}

func (p *prefetcher) finish(job prefetchJob) {
	p.inFlightM.Lock()
	delete(p.inFlight, prefetchJobKey(job))
	p.inFlightM.Unlock()
}

// allow consumes one unit of the background budget. It is checked immediately
// before each upstream call so cached targets never spend budget.
func (p *prefetcher) allow() bool {
	if p.budget == nil {
		return true
	}
	allowed, _ := p.budget.allow(prefetchBudgetKey)
	return allowed
}

// prefetchLyrics caches LRCLIB lyrics for the song. It skips when lyrics are
// already stored, when the lookup was recently memoized as a miss, or when the
// background budget is exhausted.
func (p *prefetcher) prefetchLyrics(ctx context.Context, job prefetchJob) {
	if p.lrclibClient == nil || p.metadataDB == nil || p.lyricsDB == nil {
		return
	}
	if job.trackName == "" {
		return
	}
	_, lyrics, err := db.FindTrackExact(ctx, p.metadataDB, p.lyricsDB, job.trackName, job.artistName, job.albumName, job.duration)
	if err == nil && lyricsAvailable(lyrics) {
		return
	}
	if err == nil {
		// A metadata row can exist before lyrics have been fetched; treat it as
		// a miss, mirroring the lyrics handler.
		err = sql.ErrNoRows
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return
	}
	missKey := lyricsMissKey(job.trackName, job.artistName, job.albumName, job.duration)
	if p.lyricsMisses.Has(missKey, time.Now()) {
		return
	}
	if !p.allow() {
		return
	}
	remote, remoteErr := lookupRemoteLyrics(ctx, p.lrclibClient, job.trackName, job.artistName, job.albumName, job.duration)
	if remoteErr != nil {
		// Memoize genuine misses so the budget is not spent on them again for
		// the TTL window. Artist-less lookups resolve through search and are not
		// memoized, mirroring the lyrics handler.
		if errors.Is(remoteErr, lrclib.ErrNotFound) && job.artistName != "" {
			p.lyricsMisses.Set(missKey, time.Now())
		}
		return
	}
	if !remoteLyricsAvailable(remote) {
		return
	}
	cachedTrack := db.Track{
		Name:       firstNonEmpty(remote.TrackName, job.trackName),
		ArtistName: firstNonEmpty(remote.ArtistName, job.artistName),
		AlbumName:  firstNonEmpty(remote.AlbumName, job.albumName),
		Duration:   remote.Duration,
		Source:     "lrclib_fallback",
	}
	if cachedTrack.Duration <= 0 {
		cachedTrack.Duration = job.duration
	}
	if _, _, err := db.InsertTrackWithLyrics(ctx, p.metadataDB, p.lyricsDB, cachedTrack, db.Lyrics{
		PlainLyrics:  remote.PlainLyrics,
		SyncedLyrics: remote.SyncedLyrics,
		Instrumental: remote.Instrumental,
		Source:       "lrclib_fallback",
	}); err != nil {
		p.logger.Debug("prefetch lyrics store failed", "error", err, "track", job.trackName, "artist", job.artistName)
	}
}

// prefetchAlbumCover caches album artwork once the song's album is known. It
// skips when a positive or fresh-negative row already exists.
func (p *prefetcher) prefetchAlbumCover(ctx context.Context, job prefetchJob) {
	if p.coverRes == nil || p.coverDB == nil {
		return
	}
	album := strings.TrimSpace(job.albumName)
	artist := strings.TrimSpace(job.artistName)
	if album == "" {
		return
	}
	cached, cacheErr := db.FindCoverArt(ctx, p.coverDB, db.CoverAlbum, artist, album)
	if cacheErr == nil && (cached.CoverURL != "" || checkedRecently(cached.CheckedAt)) {
		return
	}
	if cacheErr != nil && !errors.Is(cacheErr, sql.ErrNoRows) {
		return
	}
	if !p.allow() {
		return
	}
	input := cover.Input{ArtistName: artist, AlbumName: album}
	results, err := p.coverRes.Search(ctx, cover.Album, input, 50)
	if err == nil {
		results = filterCoverResults(cover.Album, input, results)
	}
	if err != nil || len(results) == 0 {
		_ = db.UpsertCoverArt(ctx, p.coverDB, db.CoverAlbum, artist, album, "", "")
		return
	}
	// Cache every plausible provider URL, not just the winner, mirroring the
	// cover handlers.
	variants := make([]db.CoverVariant, 0, len(results))
	for _, result := range results {
		variants = append(variants, db.CoverVariant{URL: result.URL, Source: result.Source})
	}
	_ = db.UpsertCoverArtVariants(ctx, p.coverDB, db.CoverAlbum, artist, album, variants)
}

// prefetchArtistCover caches artist artwork. It skips when a positive or
// fresh-negative row already exists.
func (p *prefetcher) prefetchArtistCover(ctx context.Context, job prefetchJob) {
	if p.coverRes == nil || p.coverDB == nil {
		return
	}
	artist := strings.TrimSpace(job.artistName)
	if artist == "" {
		return
	}
	cached, cacheErr := db.FindCoverArt(ctx, p.coverDB, db.CoverArtist, artist, "")
	if cacheErr == nil && (cached.CoverURL != "" || checkedRecently(cached.CheckedAt)) {
		return
	}
	if cacheErr != nil && !errors.Is(cacheErr, sql.ErrNoRows) {
		return
	}
	if !p.allow() {
		return
	}
	input := cover.Input{ArtistName: artist}
	results, err := p.coverRes.Search(ctx, cover.Artist, input, 50)
	if err == nil {
		results = filterCoverResults(cover.Artist, input, results)
	}
	if err != nil || len(results) == 0 {
		_ = db.UpsertCoverArt(ctx, p.coverDB, db.CoverArtist, artist, "", "", "")
		return
	}
	// Cache every plausible provider URL, not just the winner, mirroring the
	// cover handlers.
	variants := make([]db.CoverVariant, 0, len(results))
	for _, result := range results {
		variants = append(variants, db.CoverVariant{URL: result.URL, Source: result.Source})
	}
	_ = db.UpsertCoverArtVariants(ctx, p.coverDB, db.CoverArtist, artist, "", variants)
}

func prefetchJobKey(job prefetchJob) string {
	return strings.ToLower(strings.TrimSpace(job.trackName)) + "\x00" +
		strings.ToLower(strings.TrimSpace(job.artistName)) + "\x00" +
		strings.ToLower(strings.TrimSpace(job.albumName)) + "\x00" +
		lyricsMissDurationBucket(job.duration)
}
