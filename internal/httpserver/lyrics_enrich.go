package httpserver

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/richlyrics"
)

const enrichBudgetKey = "__enrich__"

type enrichJob struct {
	trackID    int64
	trackName  string
	artistName string
	albumName  string
	duration   float64
	videoID    string
	isrc       string
}

type enricher struct {
	metadataDB   *sql.DB
	lyricsDB     *sql.DB
	providers    *lyricsProviders
	lyricsMisses *lyricsMissCache
	logger       *slog.Logger
	budget       *rateLimiter
	workers      int
	queue        chan enrichJob
	inFlight     map[string]struct{}
	inFlightM    sync.Mutex
	stop         context.CancelFunc
	stopped      chan struct{}
	stopOnce     sync.Once
}

func newEnricher(cfg config.Config, metadataDB, lyricsDB *sql.DB, providers *lyricsProviders, misses *lyricsMissCache, logger *slog.Logger) *enricher {
	if !cfg.EnrichEnabled {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	budgetCfg := cfg
	budgetCfg.RateLimitPerSec = 1 << 30
	budgetCfg.RateLimitPerMin = cfg.EnrichPerMin
	if budgetCfg.RateLimitPerMin < 1 {
		budgetCfg.RateLimitPerMin = 1
	}
	workers := cfg.EnrichConcurrency
	if workers < 1 {
		workers = 1
	}
	queueSize := cfg.EnrichQueueSize
	if queueSize < 1 {
		queueSize = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &enricher{
		metadataDB:   metadataDB,
		lyricsDB:     lyricsDB,
		providers:    providers,
		lyricsMisses: misses,
		logger:       logger,
		budget:       newRateLimiter(budgetCfg),
		workers:      workers,
		queue:        make(chan enrichJob, queueSize),
		inFlight:     make(map[string]struct{}),
		stop:         cancel,
		stopped:      make(chan struct{}),
	}
	go e.run(ctx)
	return e
}

func (e *enricher) run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < e.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-e.queue:
					e.process(ctx, job)
				}
			}
		}()
	}
	wg.Wait()
	close(e.stopped)
}

func (e *enricher) Stop() {
	if e == nil {
		return
	}
	e.stopOnce.Do(func() {
		e.stop()
		<-e.stopped
		e.budget.Stop()
	})
}

func (e *enricher) Enqueue(track *db.Track, videoID string) {
	if e == nil || track == nil || track.ID <= 0 {
		return
	}
	videoID = sanitizeVideoID(videoID)
	key := enrichJobKey(track.ID, videoID)
	e.inFlightM.Lock()
	if _, ok := e.inFlight[key]; ok {
		e.inFlightM.Unlock()
		return
	}
	e.inFlight[key] = struct{}{}
	e.inFlightM.Unlock()
	job := enrichJob{
		trackID:    track.ID,
		trackName:  track.Name,
		artistName: track.ArtistName,
		albumName:  track.AlbumName,
		duration:   track.Duration,
		videoID:    videoID,
		isrc:       track.ISRC,
	}
	select {
	case e.queue <- job:
	default:
		e.inFlightM.Lock()
		delete(e.inFlight, key)
		e.inFlightM.Unlock()
		e.logger.Debug("enrich queue full, dropping", "track", track.Name, "artist", track.ArtistName)
	}
}

func (e *enricher) finish(job enrichJob) {
	e.inFlightM.Lock()
	delete(e.inFlight, enrichJobKey(job.trackID, job.videoID))
	e.inFlightM.Unlock()
}

func enrichJobKey(trackID int64, videoID string) string {
	base := strconv.FormatInt(trackID, 10)
	if videoID == "" {
		return base
	}
	return base + "\x00" + videoID
}

func (e *enricher) allow() bool {
	if e.budget == nil {
		return true
	}
	allowed, _ := e.budget.allow(enrichBudgetKey)
	return allowed
}

func hasRichSource(sources map[string]struct{}, source string) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	_, ok := sources[source]
	return ok
}

func isEnrichWordEmpty(trackID int64, provider string, lyricsDB *sql.DB) bool {
	if trackID <= 0 || lyricsDB == nil {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	// Only word-synced providers can be empty-word stale.
	if provider != "paxsenix" && provider != "betterlyrics" && provider != "lyricsplus" && provider != "unison" {
		return false
	}
	// Direct provider row check.
	var content, format, syncType string
	err := lyricsDB.QueryRow(`SELECT content, format, sync_type FROM lyrics_sync_variants WHERE track_id=? AND source=? AND sync_type='word' LIMIT 1`, trackID, provider).Scan(&content, &format, &syncType)
	if err != nil {
		return false
	}
	// Reuse existing empty-word detector (via compact parse).
	// We avoid importing parse function directly – inline check: if content is JSON with words empty.
	rich := &db.RichLyrics{TrackID: trackID, Content: content, Format: format, SyncType: syncType, Source: provider}
	return isWordRichEmpty(rich)
}

func (e *enricher) process(ctx context.Context, job enrichJob) {
	defer e.finish(job)
	if e.providers == nil || e.lyricsDB == nil {
		return
	}
	existingSources := make(map[string]struct{})
	if job.trackID > 0 {
		if sources, err := db.ListRichLyricsSources(ctx, e.lyricsDB, job.trackID); err == nil {
			for k := range sources {
				existingSources[k] = struct{}{}
			}
		}
	}
	var needsSynced bool
	if e.metadataDB != nil {
		if _, lyrics, err := db.FindTrackExact(ctx, e.metadataDB, e.lyricsDB, job.trackName, job.artistName, job.albumName, job.duration); err == nil {
			if lyrics != nil && !lyrics.HasSynced && !lyrics.Instrumental {
				needsSynced = true
			}
		}
	}

	type spec struct {
		name    string
		enabled bool
		has     bool
		needVid bool
	}
	specs := []spec{
		{"betterlyrics", e.providers.betterEnabled && e.providers.better != nil, hasRichSource(existingSources, "betterlyrics"), false},
		{"paxsenix", e.providers.paxsenixEnabled && e.providers.paxsenix != nil, hasRichSource(existingSources, "paxsenix"), false},
		{"lyricsplus", e.providers.lyricsPlusEnabled && e.providers.lyricsPlus != nil, hasRichSource(existingSources, "lyricsplus"), false},
		{"unison", e.providers.richEnabled && e.providers.rich != nil, hasRichSource(existingSources, "unison"), false},
		{"apple_music", e.providers.appleEnabled && e.providers.apple != nil, hasRichSource(existingSources, "apple_music"), false},
		{"kugou", e.providers.kugouEnabled && e.providers.kugou != nil, false, false},
		{"zemer", e.providers.zemerEnabled && e.providers.zemer != nil, false, true},
		{"youtube", e.providers.tube != nil && e.providers.tubeLyricsEnabled, false, true},
		{"youtube_subtitle", e.providers.tube != nil && e.providers.tubeSubtitleEnabled, false, true},
	}
	for i := range specs {
		switch specs[i].name {
		case "kugou":
			if !needsSynced {
				specs[i].enabled = false
			}
		case "zemer", "youtube", "youtube_subtitle":
			if job.videoID == "" {
				specs[i].enabled = false
			}
		}
		if specs[i].has {
			// Empty-word word variants don't count as having rich – allow retry with better source.
			if isEnrichWordEmpty(job.trackID, specs[i].name, e.lyricsDB) {
				specs[i].has = false
			} else {
				specs[i].enabled = false
			}
		}
		if specs[i].enabled && e.lyricsMisses != nil && e.lyricsMisses.HasProvider(specs[i].name, job.trackName, job.artistName, job.albumName, job.videoID, time.Now()) {
			specs[i].enabled = false
		}
		if specs[i].enabled && job.trackID > 0 && e.lyricsDB != nil {
			if recent, err := db.HasRecentProviderFetch(ctx, e.lyricsDB, job.trackID, specs[i].name, db.ProviderFetchStaleTTL); err == nil && recent {
				// If the last fetch was word-empty, don't treat it as recent success – retry quickly.
				if isEnrichWordEmpty(job.trackID, specs[i].name, e.lyricsDB) {
					// Allow immediate retry by ignoring the stale TTL.
				} else {
					specs[i].enabled = false
				}
			}
		}
	}
	hasAny := false
	for _, s := range specs {
		if s.enabled {
			hasAny = true
			break
		}
	}
	if !hasAny {
		return
	}

	bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range specs {
		if !s.enabled {
			continue
		}
		if !e.allow() {
			break
		}
		switch s.name {
		case "betterlyrics":
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := e.providers.better.Get(bgCtx, job.trackName, job.artistName, job.albumName, job.duration)
				if err != nil {
					if e.lyricsMisses != nil {
						e.lyricsMisses.SetProvider("betterlyrics", job.trackName, job.artistName, job.albumName, job.videoID, time.Now())
					}
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "betterlyrics", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				row := &lrclib.RemoteResult{TrackName: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration, PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}
				if _, _, ok := persistProviderLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, row, job.trackName, job.artistName, job.albumName, job.duration, "betterlyrics"); !ok {
					return
				}
				if remote.WordSynced && strings.TrimSpace(remote.TTML) != "" {
					_, _, _ = persistRemoteRichLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, &richlyrics.Result{Content: remote.TTML, Format: "ttml", SyncType: "word", Source: "betterlyrics"}, job.trackName, job.artistName, job.albumName, job.duration)
				}
				if e.lyricsDB != nil && job.trackID > 0 {
					_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "betterlyrics", true)
				}
			}()
		case "paxsenix":
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := e.providers.paxsenix.Get(bgCtx, job.trackName, job.artistName, job.albumName)
				if err != nil {
					if e.lyricsMisses != nil {
						e.lyricsMisses.SetProvider("paxsenix", job.trackName, job.artistName, job.albumName, job.videoID, time.Now())
					}
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "paxsenix", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				row := &lrclib.RemoteResult{
					TrackName: firstNonEmpty(remote.TrackName, job.trackName), ArtistName: firstNonEmpty(remote.ArtistName, job.artistName),
					AlbumName: firstNonEmpty(remote.AlbumName, job.albumName), Duration: remote.Duration,
					PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
				}
				if row.Duration <= 0 {
					row.Duration = job.duration
				}
				if _, _, ok := persistProviderLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, row, job.trackName, job.artistName, job.albumName, job.duration, "paxsenix"); !ok {
					return
				}
				if remote.WordSynced && strings.TrimSpace(remote.TTML) != "" {
					_, _, _ = persistRemoteRichLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, &richlyrics.Result{Content: remote.TTML, Format: "ttml", SyncType: "word", Source: "paxsenix"}, job.trackName, job.artistName, job.albumName, job.duration)
				}
				if e.lyricsDB != nil && job.trackID > 0 {
					_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "paxsenix", true)
				}
			}()
		case "lyricsplus":
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := e.providers.lyricsPlus.Get(bgCtx, job.trackName, job.artistName, job.albumName, job.duration, job.isrc)
				if err != nil {
					if e.lyricsMisses != nil {
						e.lyricsMisses.SetProvider("lyricsplus", job.trackName, job.artistName, job.albumName, job.videoID, time.Now())
					}
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "lyricsplus", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				row := &lrclib.RemoteResult{
					TrackName: firstNonEmpty(remote.TrackName, job.trackName), ArtistName: firstNonEmpty(remote.ArtistName, job.artistName),
					AlbumName: firstNonEmpty(remote.AlbumName, job.albumName), Duration: job.duration,
					PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
				}
				if _, _, ok := persistProviderLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, row, job.trackName, job.artistName, job.albumName, job.duration, "lyricsplus"); !ok {
					return
				}
				if remote.WordSynced {
					if strings.TrimSpace(remote.TTML) != "" {
						_, _, _ = persistRemoteRichLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, &richlyrics.Result{Content: remote.TTML, Format: "ttml", SyncType: "word", Source: "lyricsplus"}, job.trackName, job.artistName, job.albumName, job.duration)
					} else if strings.TrimSpace(remote.RichJSON) != "" {
						rich := db.RichLyrics{TrackID: job.trackID, Content: remote.RichJSON, Format: "json", SyncType: "word", Source: "lyricsplus"}
						_ = db.UpsertRichLyrics(bgCtx, e.lyricsDB, rich)
					}
				}
				if e.lyricsDB != nil && job.trackID > 0 {
					_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "lyricsplus", true)
				}
			}()
		case "unison":
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := e.providers.rich.Get(bgCtx, job.trackName, job.artistName, job.albumName)
				if err != nil {
					if e.lyricsMisses != nil {
						e.lyricsMisses.SetProvider("unison", job.trackName, job.artistName, job.albumName, job.videoID, time.Now())
					}
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "unison", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				if _, _, ok := persistRemoteRichLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, remote, job.trackName, job.artistName, job.albumName, job.duration); ok {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "unison", true)
					}
				}
			}()
		case "apple_music":
			wg.Add(1)
			go func() {
				defer wg.Done()
				trk, err := e.providers.apple.SearchTrack(bgCtx, job.trackName, job.artistName, job.albumName)
				if err != nil {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "apple_music", false)
					}
					return
				}
				remote, err := e.providers.apple.GetLyrics(bgCtx, trk.ID)
				if err != nil {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "apple_music", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				if _, _, ok := persistRemoteRichLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, &richlyrics.Result{Content: remote.Content, Format: remote.Format, SyncType: remote.SyncType, Source: remote.Source}, trk.Name, trk.ArtistName, trk.AlbumName, trk.Duration); ok {
					_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "apple_music", true)
				}
			}()
		case "kugou":
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := e.providers.kugou.Get(bgCtx, job.trackName, job.artistName, job.albumName, job.duration)
				if err != nil {
					if e.lyricsMisses != nil {
						e.lyricsMisses.SetProvider("kugou", job.trackName, job.artistName, job.albumName, job.videoID, time.Now())
					}
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "kugou", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				row := &lrclib.RemoteResult{
					TrackName: firstNonEmpty(remote.TrackName, job.trackName), ArtistName: firstNonEmpty(remote.ArtistName, job.artistName),
					AlbumName: firstNonEmpty(remote.AlbumName, job.albumName), Duration: remote.Duration,
					PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
				}
				if row.Duration <= 0 {
					row.Duration = job.duration
				}
				if _, _, ok := persistProviderLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, row, job.trackName, job.artistName, job.albumName, job.duration, "kugou"); ok {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "kugou", true)
					}
				}
			}()
		case "zemer":
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := e.providers.zemer.Get(bgCtx, job.videoID)
				if err != nil {
					if e.lyricsMisses != nil {
						e.lyricsMisses.SetProvider("zemer", job.trackName, job.artistName, job.albumName, job.videoID, time.Now())
					}
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "zemer", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				row := &lrclib.RemoteResult{TrackName: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration, PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}
				if _, _, ok := persistProviderLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, row, job.trackName, job.artistName, job.albumName, job.duration, "zemer"); ok {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "zemer", true)
					}
				}
			}()
		case "youtube":
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := e.providers.tube.GetOfficialLyrics(bgCtx, job.videoID)
				if err != nil {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "youtube", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				row := &lrclib.RemoteResult{TrackName: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration, PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}
				if _, _, ok := persistProviderLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, row, job.trackName, job.artistName, job.albumName, job.duration, "youtube"); ok {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "youtube", true)
					}
				}
			}()
		case "youtube_subtitle":
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := e.providers.tube.GetTranscript(bgCtx, job.videoID)
				if err != nil {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "youtube_subtitle", false)
					}
					return
				}
				existing := &db.Track{ID: job.trackID, Name: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration}
				row := &lrclib.RemoteResult{TrackName: job.trackName, ArtistName: job.artistName, AlbumName: job.albumName, Duration: job.duration, PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}
				if _, _, ok := persistProviderLyrics(bgCtx, e.metadataDB, e.lyricsDB, existing, row, job.trackName, job.artistName, job.albumName, job.duration, "youtube_subtitle"); ok {
					if e.lyricsDB != nil && job.trackID > 0 {
						_ = db.UpsertProviderFetch(bgCtx, e.lyricsDB, job.trackID, "youtube_subtitle", true)
					}
				}
			}()
		}
	}
	wg.Wait()
}
