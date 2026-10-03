package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/applemusic"
	"github.com/sillygru/music-utils/internal/betterlyrics"
	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/kugou"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/lyricsplus"
	"github.com/sillygru/music-utils/internal/musixmatch"
	"github.com/sillygru/music-utils/internal/pacer"
	"github.com/sillygru/music-utils/internal/paxsenix"
	"github.com/sillygru/music-utils/internal/upstream"
	"github.com/sillygru/music-utils/internal/version"
)

// lyricsHit is one provider's usable answer for a track.
//
// It carries the provider name because a stored row keeps it in its source column:
// an alternative that lost the served pointer is still identifiable on disk, which
// is what makes a later quality rule or a provider outage recoverable from rows
// that are already there.
type lyricsHit struct {
	provider string
	lyrics   *db.Lyrics
}

// providerLookup is the result of asking exactly one provider about one track.
type providerLookup struct {
	// lyrics is the provider's answer when it returned usable lyrics.
	lyrics *db.Lyrics
	// outcome classifies the answer. It is what decides both the lane's counters
	// and whether the track may eventually be recorded as answered.
	outcome Outcome
	// retryAfter carries the advertised cooldown when the provider throttled.
	retryAfter time.Duration
	// rejected reports that a provider refused this caller outright.
	rejected  bool
	rejection string
	// err is the failure behind a non-definitive outcome, kept for diagnostics.
	err error
}

// lyricsProvider is one upstream in the chain.
type lyricsProvider struct {
	name string
	get  func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error)
}

// lyricsBackfillResolver is the provider set plus whether cross-process
// coordination was available, which decides both the pacers in use and whether
// the run can be announced to the live server.
//
// The ordering is by coverage, and it survives the split into per-provider lookups
// because it decides the one thing arrival order no longer can: which of two
// equally good answers is served.
//
// Every configured provider is asked about every track, one at a time rather than
// all at once. A fan-out across six upstreams put six requests per track into the
// air simultaneously and bought nothing except a queue in front of each provider:
// they are already limited to one request per interval, so the extra concurrency
// could not turn into extra throughput. Asking in turn costs the same requests at
// the same rate and leaves the run free to spread its songs across providers, so
// the parallelism goes into distinct upstreams instead of piling onto one.
//
// A batch run is not answering a request, so it can afford the upstream calls a
// sequential walk would skip, and the alternatives it leaves on disk are the
// point: the live path serves one of them, but a provider outage, a bad scrape,
// or a later quality rule all become recoverable from rows already stored.
//
// YouTube is absent because it is keyed by video ID, which a batch run has no way
// to obtain: the live path takes it from the query string. Every provider here is
// reachable from a title, artist, album, and duration alone.
//
// Word-level rich variants are absent too, which is why the set carries no Unison
// and no Apple Music: both answer only with TTML, and the live path compacts TTML
// into a canonical form before storing it. A job writing the raw payload would
// create rows in a different shape than the one path that reads them.
type lyricsBackfillResolver struct {
	providers []lyricsProvider
	shared    bool
}

// coordinated reports whether the shared upstream pacers and the job coordination
// table are usable, so a caller can skip registration rather than repeating a
// failure it already knows about.
func (b *lyricsBackfillResolver) coordinated() bool { return b != nil && b.shared }

// count reports how many providers a run has to get through per track.
func (b *lyricsBackfillResolver) count() int {
	if b == nil {
		return 0
	}
	return len(b.providers)
}

func (b *lyricsBackfillResolver) add(name string, get func(context.Context, db.LyricsWork) (*db.Lyrics, error)) {
	b.providers = append(b.providers, lyricsProvider{name: name, get: get})
}

// names lists the configured providers in the order a track is worked through,
// which is also the order ties between equally good answers are broken in.
func (b *lyricsBackfillResolver) names() []string {
	if b == nil {
		return nil
	}
	names := make([]string, 0, len(b.providers))
	for _, provider := range b.providers {
		names = append(names, provider.name)
	}
	return names
}

// Lookup asks one provider about one track and classifies the answer.
//
// Nothing is inferred from the other providers here. Whether the track ends up
// answered is a question about the whole set, and it is asked once the last one
// has replied; this returns only what this provider said, so a caller can tell a
// track that has no lyrics anywhere from one that has not been fully asked yet.
func (b *lyricsBackfillResolver) Lookup(ctx context.Context, index int, work db.LyricsWork) providerLookup {
	if index < 0 || index >= len(b.providers) {
		return providerLookup{outcome: OutcomeFailed, err: fmt.Errorf("provider %d is not configured", index)}
	}
	provider := b.providers[index]
	if err := ctx.Err(); err != nil {
		// Checked before the request goes out, so an interrupted run spends
		// nothing upstream and cannot leave a partial answer to interpret.
		return providerLookup{outcome: OutcomeFailed, err: err}
	}

	lyrics, err := provider.get(ctx, work)
	switch {
	case err != nil && isNotFound(err):
		// A definitive negative, which is as final as an answer and records as one.
		return providerLookup{outcome: OutcomeMissing}
	case err != nil:
		result := providerLookup{outcome: OutcomeFailed, err: fmt.Errorf("%s: %w", provider.name, err)}
		// Throttling is keyed on the error being a rate limit rather than on a
		// cooldown having been advertised. A 429 with no Retry-After is still a
		// rate limit, and it is exactly the case where the run knows to wait and
		// retry rather than write the provider off.
		if upstream.IsRateLimited(err) {
			result.outcome = OutcomeThrottled
			result.retryAfter = upstream.RetryAfterFor(err)
		}
		if upstream.IsRejected(err) {
			result.rejected = true
			result.rejection = upstream.RejectionSummary(err)
		}
		return result
	case usableLyrics(lyrics):
		return providerLookup{lyrics: lyrics, outcome: OutcomeSucceeded}
	default:
		// Reached the provider but it had nothing to return.
		return providerLookup{outcome: OutcomeMissing}
	}
}

// usableLyrics reports whether a provider's result is worth storing. It matches
// what the live request path treats as available, so anything the job stores is
// something the server would have served.
func usableLyrics(lyrics *db.Lyrics) bool {
	return lyrics != nil && (lyrics.Instrumental || lyrics.PlainLyrics != "" || lyrics.SyncedLyrics != "")
}

// providerMisses is every lyrics client's own "no such song" sentinel.
//
// Each client package defines its own, and a batch run has to recognize all of
// them to tell a definitive negative apart from a failure. That distinction is the
// difference between recording a track as having no lyrics and recording it as
// never having been asked.
var providerMisses = []error{
	lrclib.ErrNotFound,
	betterlyrics.ErrNotFound,
	kugou.ErrNotFound,
	paxsenix.ErrNotFound,
	lyricsplus.ErrNotFound,
	musixmatch.ErrNotFound,
	applemusic.ErrNotFound,
}

// isNotFound reports whether err is a provider's own "no such song" answer rather
// than a failure.
func isNotFound(err error) bool {
	for _, miss := range providerMisses {
		if errors.Is(err, miss) {
			return true
		}
	}
	return false
}

// providerUserAgent falls back to the server's own identifier when a provider is
// configured without one, matching what the live request path sends.
func providerUserAgent(value string) string {
	if value != "" {
		return value
	}
	return "music-utils/" + version.Version + " (+https://gru0.dev)"
}

// newLyricsBackfillResolver orders the providers by coverage. The order does not
// decide who is asked, since everyone is; it decides which of two equal answers
// wins.
//
// The pacers are shared with the live server through the metadata database and run
// in the job class, so this job only reaches an upstream during a window in which
// no real request is pending. Live traffic always wins. The job's own interval is
// config's, not a constant here: it is the same number the live server puts on the
// lease as the job side, and a lease whose two halves disagree is not a shared
// budget. UPSTREAM_PACE_MS never reaches this path, because a job working through
// a library must not inherit the live path's rate in either direction.
//
// It is the lyrics rate rather than the metadata job's, because this job asks every
// provider about every song and the metadata job asks two upstreams about one: one
// rate for both would spend the lyrics providers' budget several times over.
//
// It returns nil when no provider could be built at all, so the caller can report
// that as a configuration problem rather than running a job that cannot do
// anything.
func newLyricsBackfillResolver(cfg config.Config, metadataDB *sql.DB, idleGap time.Duration, errOut io.Writer) *lyricsBackfillResolver {
	resolver := &lyricsBackfillResolver{}
	jobPace := cfg.UpstreamLyricsJobInterval()

	// Coordination is established once, on the first provider that asks for a
	// shared pacer. Doing it eagerly would report a failure for a run whose
	// providers are all disabled, and a disabled provider must not reserve a name
	// on the shared lease either.
	coordinationTried := false
	share := func(name string) pacer.Waiter {
		if metadataDB == nil {
			return pacer.New(jobPace)
		}
		if !coordinationTried {
			coordinationTried = true
			if err := db.EnsureCoordination(context.Background(), metadataDB); err != nil {
				fmt.Fprintf(errOut, "shared upstream pacing unavailable, pacing locally: %v\n", err)
			} else {
				resolver.shared = true
			}
		}
		if !resolver.shared {
			return pacer.New(jobPace)
		}
		return pacer.NewShared(metadataDB, name, jobPace, jobPace, idleGap).ForJob()
	}

	if cfg.LRCLIBFallbackEnabled {
		client, err := lrclib.NewWithPacer(cfg.LRCLIBBaseURL, providerUserAgent(cfg.LRCLIBUserAgent),
			time.Duration(cfg.LRCLIBTimeoutMS)*time.Millisecond, share(lrclib.LeaseName))
		if err != nil {
			fmt.Fprintf(errOut, "skip LRCLIB provider: %v\n", err)
		} else {
			resolver.add("lrclib", func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error) {
				remote, err := client.GetWithFallbacks(ctx, work.Name, work.Artist, work.Album, work.Duration)
				if err != nil {
					return nil, err
				}
				return &db.Lyrics{
					PlainLyrics:  remote.PlainLyrics,
					SyncedLyrics: remote.SyncedLyrics,
					Instrumental: remote.Instrumental,
				}, nil
			})
		}
	}

	if cfg.BetterLyricsEnabled {
		client, err := betterlyrics.NewWithPacer(cfg.BetterLyricsBaseURL, providerUserAgent(cfg.BetterLyricsUserAgent),
			time.Duration(cfg.BetterLyricsTimeoutMS)*time.Millisecond, share(betterlyrics.LeaseName))
		if err != nil {
			fmt.Fprintf(errOut, "skip BetterLyrics provider: %v\n", err)
		} else {
			resolver.add("betterlyrics", func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error) {
				remote, err := client.Get(ctx, work.Name, work.Artist, work.Album, work.Duration)
				if err != nil {
					return nil, err
				}
				return &db.Lyrics{PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}, nil
			})
		}
	}

	if cfg.KugouEnabled {
		client, err := kugou.NewWithPacer(cfg.KugouSearchBaseURL, cfg.KugouLyricsBaseURL, providerUserAgent(cfg.KugouUserAgent),
			time.Duration(cfg.KugouTimeoutMS)*time.Millisecond, share(kugou.LeaseName))
		if err != nil {
			fmt.Fprintf(errOut, "skip KuGou provider: %v\n", err)
		} else {
			resolver.add("kugou", func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error) {
				remote, err := client.Get(ctx, work.Name, work.Artist, work.Album, work.Duration)
				if err != nil {
					return nil, err
				}
				return &db.Lyrics{PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}, nil
			})
		}
	}

	if cfg.PaxsenixEnabled {
		client, err := paxsenix.NewWithPacer(cfg.PaxsenixProxyBaseURL, cfg.PaxsenixAppleBaseURL, providerUserAgent(cfg.PaxsenixUserAgent),
			time.Duration(cfg.PaxsenixTimeoutMS)*time.Millisecond, share(paxsenix.LeaseName))
		if err != nil {
			fmt.Fprintf(errOut, "skip Paxsenix provider: %v\n", err)
		} else {
			resolver.add("paxsenix", func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error) {
				remote, err := client.Get(ctx, work.Name, work.Artist, work.Album)
				if err != nil {
					return nil, err
				}
				return &db.Lyrics{PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}, nil
			})
		}
	}

	if cfg.LyricsPlusEnabled {
		mirrors := cfg.LyricsPlusMirrors
		if len(mirrors) == 0 {
			mirrors = lyricsplus.DefaultMirrors()
		}
		client, err := lyricsplus.NewWithPacer(cfg.LyricsPlusAPIBaseURL, mirrors, providerUserAgent(cfg.LyricsPlusUserAgent),
			time.Duration(cfg.LyricsPlusTimeoutMS)*time.Millisecond, share(lyricsplus.LeaseName))
		if err != nil {
			fmt.Fprintf(errOut, "skip LyricsPlus provider: %v\n", err)
		} else {
			resolver.add("lyricsplus", func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error) {
				remote, err := client.Get(ctx, work.Name, work.Artist, work.Album, work.Duration, work.ISRC)
				if err != nil {
					return nil, err
				}
				return &db.Lyrics{PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}, nil
			})
		}
	}

	// Apple Music is absent because it only ever returns a TTML rich-sync
	// payload (internal/applemusic/client.go returns Format "ttml"). The live path
	// compacts that into the canonical rich form before storing it, and a job
	// writing the raw payload would create rows in a different shape than the one
	// path that reads them.
	//
	// Musixmatch is the last name-keyed provider. It answers with plain lyrics
	// only, so it can fill a track that has nothing, but it can never improve one
	// that already has synced lyrics. Its position no longer decides whether it is
	// asked at all, only how a tie against another plain-only answer is broken, so
	// it stays last: when two providers have nothing but plain lyrics, the one with
	// better coverage should be the one served.
	if cfg.MusixmatchEnabled {
		client, err := musixmatch.NewWithPacer(cfg.MusixmatchBaseURL, cfg.MusixmatchAPIKey, providerUserAgent(cfg.MusixmatchUserAgent),
			time.Duration(cfg.MusixmatchTimeoutMS)*time.Millisecond, share(musixmatch.LeaseName))
		if err != nil {
			fmt.Fprintf(errOut, "skip Musixmatch provider: %v\n", err)
		} else {
			resolver.add("musixmatch", func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error) {
				track, err := client.SearchTrack(ctx, work.Name, work.Artist, work.Album)
				if err != nil {
					return nil, err
				}
				remote, err := client.GetLyrics(ctx, track.CommonTrackID, work.ISRC)
				if err != nil {
					return nil, err
				}
				return &db.Lyrics{PlainLyrics: remote.PlainLyrics}, nil
			})
		}
	}

	if len(resolver.providers) == 0 {
		return nil
	}
	return resolver
}

// describeProviders renders the provider set for a run header.
func describeProviders(resolver *lyricsBackfillResolver) string {
	names := resolver.names()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
