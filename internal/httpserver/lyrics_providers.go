package httpserver

import (
	"log/slog"
	"time"

	"github.com/sillygru/music-utils/internal/applemusic"
	"github.com/sillygru/music-utils/internal/betterlyrics"
	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/innertube"
	"github.com/sillygru/music-utils/internal/kugou"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/lyricsplus"
	"github.com/sillygru/music-utils/internal/musixmatch"
	"github.com/sillygru/music-utils/internal/paxsenix"
	"github.com/sillygru/music-utils/internal/richlyrics"
	"github.com/sillygru/music-utils/internal/version"
	"github.com/sillygru/music-utils/internal/zemer"
)

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
	zemer      *zemer.Client
	tube       *innertube.Client

	lrclibEnabled       bool
	richEnabled         bool
	appleEnabled        bool
	musixEnabled        bool
	betterEnabled       bool
	kugouEnabled        bool
	paxsenixEnabled     bool
	lyricsPlusEnabled   bool
	zemerEnabled        bool
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
	zemerClient *zemer.Client,
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
		zemer:      zemerClient,
		tube:       tubeClient,

		lrclibEnabled:       cfg.LRCLIBFallbackEnabled,
		richEnabled:         cfg.RichLyricsEnabled,
		appleEnabled:        cfg.AppleMusicEnabled,
		musixEnabled:        cfg.MusixmatchEnabled,
		betterEnabled:       cfg.BetterLyricsEnabled,
		kugouEnabled:        cfg.KugouEnabled,
		paxsenixEnabled:     cfg.PaxsenixEnabled,
		lyricsPlusEnabled:   cfg.LyricsPlusEnabled,
		zemerEnabled:        cfg.ZemerEnabled,
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
	if p.zemerEnabled && p.zemer != nil {
		return true
	}
	if p.tube != nil && (p.tubeLyricsEnabled || p.tubeSubtitleEnabled) {
		return true
	}
	return false
}

func newBetterLyricsClient(cfg config.Config, logger *slog.Logger) *betterlyrics.Client {
	if !cfg.BetterLyricsEnabled {
		return nil
	}
	client, err := betterlyrics.New(cfg.BetterLyricsBaseURL, defaultUserAgent(cfg.BetterLyricsUserAgent), time.Duration(cfg.BetterLyricsTimeoutMS)*time.Millisecond)
	if err != nil {
		logger.Error("configure BetterLyrics client", "error", err)
		return nil
	}
	return client
}

func newKugouClient(cfg config.Config, logger *slog.Logger) *kugou.Client {
	if !cfg.KugouEnabled {
		return nil
	}
	client, err := kugou.New(cfg.KugouSearchBaseURL, cfg.KugouLyricsBaseURL, defaultUserAgent(cfg.KugouUserAgent), time.Duration(cfg.KugouTimeoutMS)*time.Millisecond)
	if err != nil {
		logger.Error("configure KuGou client", "error", err)
		return nil
	}
	return client
}

func newPaxsenixClient(cfg config.Config, logger *slog.Logger) *paxsenix.Client {
	if !cfg.PaxsenixEnabled {
		return nil
	}
	client, err := paxsenix.New(cfg.PaxsenixProxyBaseURL, cfg.PaxsenixAppleBaseURL, defaultUserAgent(cfg.PaxsenixUserAgent), time.Duration(cfg.PaxsenixTimeoutMS)*time.Millisecond)
	if err != nil {
		logger.Error("configure Paxsenix client", "error", err)
		return nil
	}
	return client
}

func newLyricsPlusClient(cfg config.Config, logger *slog.Logger) *lyricsplus.Client {
	if !cfg.LyricsPlusEnabled {
		return nil
	}
	mirrors := cfg.LyricsPlusMirrors
	if len(mirrors) == 0 {
		mirrors = lyricsplus.DefaultMirrors()
	}
	client, err := lyricsplus.New(cfg.LyricsPlusAPIBaseURL, mirrors, defaultUserAgent(cfg.LyricsPlusUserAgent), time.Duration(cfg.LyricsPlusTimeoutMS)*time.Millisecond)
	if err != nil {
		logger.Error("configure LyricsPlus client", "error", err)
		return nil
	}
	return client
}

func newZemerClient(cfg config.Config, logger *slog.Logger) *zemer.Client {
	if !cfg.ZemerEnabled {
		return nil
	}
	client, err := zemer.New(cfg.ZemerBaseURL, defaultUserAgent(cfg.ZemerUserAgent), time.Duration(cfg.ZemerTimeoutMS)*time.Millisecond)
	if err != nil {
		logger.Error("configure Zemer client", "error", err)
		return nil
	}
	return client
}

func newInnerTubeClient(cfg config.Config, logger *slog.Logger) *innertube.Client {
	if !cfg.YouTubeLyricsEnabled && !cfg.YouTubeSubtitleEnabled {
		return nil
	}
	client, err := innertube.New(cfg.YouTubeBaseURL, cfg.YouTubeAPIKey, defaultUserAgent(cfg.YouTubeUserAgent), time.Duration(cfg.YouTubeTimeoutMS)*time.Millisecond)
	if err != nil {
		logger.Error("configure InnerTube client", "error", err)
		return nil
	}
	return client
}
