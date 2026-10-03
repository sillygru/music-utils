package httpserver

import (
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/applemusic"
	"github.com/sillygru/music-utils/internal/betterlyrics"
	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/innertube"
	"github.com/sillygru/music-utils/internal/kugou"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/lyricsplus"
	"github.com/sillygru/music-utils/internal/musixmatch"
	"github.com/sillygru/music-utils/internal/pacer"
	"github.com/sillygru/music-utils/internal/paxsenix"
	"github.com/sillygru/music-utils/internal/richlyrics"
	"github.com/sillygru/music-utils/internal/version"
)

// lyricsPacerFactory hands the lyrics clients the pacer they should use, putting
// each one on the shared lease the lyrics backfill also claims.
//
// A nil factory is the normal state for a server started without a metadata
// database, and the accessors then fall back to pacing each client privately. That
// is the right degradation: without the coordination table there is nothing to
// share, and private pacing is what is left rather than no pacing. The private
// pacer is still built from the same resolved interval as the shared one, so
// UPSTREAM_PACE_MS is honoured on this path rather than quietly dropped.
type lyricsPacerFactory struct {
	cfg      config.Config
	database *sql.DB
	idleGap  time.Duration
	// shared caches one lease per upstream name. Two clients pointing at the same
	// provider must not each build their own, or they would space against
	// different in-process fallbacks the moment the lease became unavailable.
	mu     sync.Mutex
	shared map[string]*pacer.Shared
}

func newLyricsPacerFactory(cfg config.Config, database *sql.DB, idleGap time.Duration) *lyricsPacerFactory {
	return &lyricsPacerFactory{
		cfg:      cfg,
		database: database,
		idleGap:  idleGap,
		shared:   make(map[string]*pacer.Shared),
	}
}

// lease returns the shared pacer for one upstream, building it on first use.
func (f *lyricsPacerFactory) lease(name string, userInterval time.Duration) *pacer.Shared {
	if f == nil || f.database == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if shared, ok := f.shared[name]; ok {
		return shared
	}
	shared := pacer.NewShared(f.database, name, userInterval, f.cfg.UpstreamLyricsJobInterval(), f.idleGap)
	f.shared[name] = shared
	return shared
}

// user returns the pacer for live lyrics traffic to this provider.
//
// The user interval is the provider's own client default, overridden by
// UPSTREAM_PACE_MS when the operator has set it, rather than the job's, because
// these two callers are doing different things and should not be held to one
// number: someone is waiting on this request, while the job is working through a
// library. They still take turns on one lease, and the job is only admitted while
// this process is idle.
func (f *lyricsPacerFactory) user(name string, userInterval time.Duration) pacer.Waiter {
	if shared := f.lease(name, userInterval); shared != nil {
		return shared.ForUser()
	}
	return pacer.New(userInterval)
}

// leases returns every shared pacer built so far, so the degradation watchdog can
// watch them alongside the metadata ones. It returns only the lyrics providers that
// were actually configured, so a disabled one does not appear to be failing.
func (f *lyricsPacerFactory) leases() []*pacer.Shared {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.shared) == 0 {
		return nil
	}
	leases := make([]*pacer.Shared, 0, len(f.shared))
	for _, shared := range f.shared {
		leases = append(leases, shared)
	}
	return leases
}

// lyricsProviders bundles every lyrics upstream client with its enable flag.
// It is threaded through the lyrics handlers and the parallel fan-out so new
// providers add fields here instead of lengthening already-long signatures.
type lyricsProviders struct {
	lrclib     *lrclib.Client
	rich       *richlyrics.Client
	apple      *applemusic.Client
	musix      *musixmatch.Client
	better     *betterlyrics.Client
	kugou      *kugou.Client
	paxsenix   *paxsenix.Client
	lyricsPlus *lyricsplus.Client
	tube       *innertube.Client

	lrclibEnabled       bool
	richEnabled         bool
	appleEnabled        bool
	musixEnabled        bool
	betterEnabled       bool
	kugouEnabled        bool
	paxsenixEnabled     bool
	lyricsPlusEnabled   bool
	tubeLyricsEnabled   bool
	tubeSubtitleEnabled bool
}

func newLyricsProviders(
	lrclibClient *lrclib.Client,
	richClient *richlyrics.Client,
	appleClient *applemusic.Client,
	musixClient *musixmatch.Client,
	betterClient *betterlyrics.Client,
	kugouClient *kugou.Client,
	paxsenixClient *paxsenix.Client,
	lyricsPlusClient *lyricsplus.Client,
	tubeClient *innertube.Client,
	cfg config.Config,
) *lyricsProviders {
	return &lyricsProviders{
		lrclib:     lrclibClient,
		rich:       richClient,
		apple:      appleClient,
		musix:      musixClient,
		better:     betterClient,
		kugou:      kugouClient,
		paxsenix:   paxsenixClient,
		lyricsPlus: lyricsPlusClient,
		tube:       tubeClient,

		lrclibEnabled:       cfg.LRCLIBFallbackEnabled,
		richEnabled:         cfg.RichLyricsEnabled,
		appleEnabled:        cfg.AppleMusicEnabled,
		musixEnabled:        cfg.MusixmatchEnabled,
		betterEnabled:       cfg.BetterLyricsEnabled,
		kugouEnabled:        cfg.KugouEnabled,
		paxsenixEnabled:     cfg.PaxsenixEnabled,
		lyricsPlusEnabled:   cfg.LyricsPlusEnabled,
		tubeLyricsEnabled:   cfg.YouTubeLyricsEnabled,
		tubeSubtitleEnabled: cfg.YouTubeSubtitleEnabled,
	}
}

func defaultUserAgent(value string) string {
	if value != "" {
		return value
	}
	return "music-utils/" + version.Version + " (+https://gru0.dev)"
}

// anyEnabled reports whether at least one exact-lookup provider can run.
func (p *lyricsProviders) anyEnabled(richRequested bool) bool {
	if p == nil {
		return false
	}
	if p.lrclibEnabled && p.lrclib != nil {
		return true
	}
	if p.richEnabled && richRequested && p.rich != nil {
		return true
	}
	if p.appleEnabled && p.apple != nil {
		return true
	}
	if p.musixEnabled && p.musix != nil {
		return true
	}
	if p.betterEnabled && p.better != nil {
		return true
	}
	if p.kugouEnabled && p.kugou != nil {
		return true
	}
	if p.paxsenixEnabled && p.paxsenix != nil {
		return true
	}
	if p.lyricsPlusEnabled && p.lyricsPlus != nil {
		return true
	}
	if p.tube != nil && (p.tubeLyricsEnabled || p.tubeSubtitleEnabled) {
		return true
	}
	return false
}

func newBetterLyricsClient(cfg config.Config, logger *slog.Logger, pace pacer.Waiter) *betterlyrics.Client {
	if !cfg.BetterLyricsEnabled {
		return nil
	}
	client, err := betterlyrics.NewWithPacer(cfg.BetterLyricsBaseURL, defaultUserAgent(cfg.BetterLyricsUserAgent), time.Duration(cfg.BetterLyricsTimeoutMS)*time.Millisecond, pace)
	if err != nil {
		logger.Error("configure BetterLyrics client", "error", err)
		return nil
	}
	return client
}

func newKugouClient(cfg config.Config, logger *slog.Logger, pace pacer.Waiter) *kugou.Client {
	if !cfg.KugouEnabled {
		return nil
	}
	client, err := kugou.NewWithPacer(cfg.KugouSearchBaseURL, cfg.KugouLyricsBaseURL, defaultUserAgent(cfg.KugouUserAgent), time.Duration(cfg.KugouTimeoutMS)*time.Millisecond, pace)
	if err != nil {
		logger.Error("configure KuGou client", "error", err)
		return nil
	}
	return client
}

func newPaxsenixClient(cfg config.Config, logger *slog.Logger, pace pacer.Waiter) *paxsenix.Client {
	if !cfg.PaxsenixEnabled {
		return nil
	}
	client, err := paxsenix.NewWithPacer(cfg.PaxsenixProxyBaseURL, cfg.PaxsenixAppleBaseURL, defaultUserAgent(cfg.PaxsenixUserAgent), time.Duration(cfg.PaxsenixTimeoutMS)*time.Millisecond, pace)
	if err != nil {
		logger.Error("configure Paxsenix client", "error", err)
		return nil
	}
	return client
}

func newLyricsPlusClient(cfg config.Config, logger *slog.Logger, pace pacer.Waiter) *lyricsplus.Client {
	if !cfg.LyricsPlusEnabled {
		return nil
	}
	mirrors := cfg.LyricsPlusMirrors
	if len(mirrors) == 0 {
		mirrors = lyricsplus.DefaultMirrors()
	}
	client, err := lyricsplus.NewWithPacer(cfg.LyricsPlusAPIBaseURL, mirrors, defaultUserAgent(cfg.LyricsPlusUserAgent), time.Duration(cfg.LyricsPlusTimeoutMS)*time.Millisecond, pace)
	if err != nil {
		logger.Error("configure LyricsPlus client", "error", err)
		return nil
	}
	return client
}

func newInnerTubeClient(cfg config.Config, logger *slog.Logger, pace pacer.Waiter) *innertube.Client {
	if !cfg.YouTubeLyricsEnabled && !cfg.YouTubeSubtitleEnabled {
		return nil
	}
	client, err := innertube.NewWithPacer(cfg.YouTubeBaseURL, cfg.YouTubeAPIKey, defaultUserAgent(cfg.YouTubeUserAgent), time.Duration(cfg.YouTubeTimeoutMS)*time.Millisecond, pace)
	if err != nil {
		logger.Error("configure InnerTube client", "error", err)
		return nil
	}
	return client
}
