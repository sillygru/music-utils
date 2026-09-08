package httpserver

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

// providerFetchLedgerTTL is how long a recorded provider attempt (success or
// miss) keeps the fan-out from re-asking that provider for the same track.
const providerFetchLedgerTTL = db.ProviderFetchStaleTTL

// lyricsCacheState is the single decision point for when a lyrics request may
// touch upstream. It classifies what the local cache can already answer and
// which providers are worth asking for what is missing.
type lyricsCacheState struct {
	// track is the locally-resolved row, or nil on a full miss.
	track *db.Track
	// lyrics is the locally-stored lyrics row, or nil.
	lyrics *db.Lyrics
	// richSources holds every rich variant source stored for the track
	// (lowercased, e.g. "unison", "lyricsplus").
	richSources map[string]struct{}
	// recentFetches maps provider name to whether the last attempt within
	// providerFetchLedgerTTL succeeded. Providers present here are skipped by
	// the fan-out regardless of outcome: successes need no refetch and misses
	// are memoized.
	recentFetches map[string]bool
	// miss memoized lookup misses keyed by lyrics miss key.
	misses *lyricsMissCache
	// missKey is the precomputed miss key for the lookup identity.
	missKey string
}

// hasLyrics reports whether the local row already answers the ordinary
// (non-rich) part of a request.
func (s *lyricsCacheState) hasLyrics() bool {
	return s.lyrics != nil && lyricsAvailable(s.lyrics)
}

// hasRich reports whether any rich variant is stored for the track.
func (s *lyricsCacheState) hasRich() bool {
	return len(s.richSources) > 0
}

// hasRichSource reports whether a specific provider's rich variant is stored.
func (s *lyricsCacheState) hasRichSource(provider string) bool {
	_, ok := s.richSources[provider]
	return ok
}

// providerRecentlyTried reports whether provider was attempted within the
// ledger TTL. skipUpstream is true for both recent successes and recent
// misses: the first is cached, the second is memoized.
func (s *lyricsCacheState) providerRecentlyTried(provider string) bool {
	_, ok := s.recentFetches[provider]
	return ok
}

// needsUpstream reports whether upstream should be consulted at all: the
// ordinary lyrics are missing, or a rich variant was requested and none is
// stored.
func (s *lyricsCacheState) needsUpstream(richRequested bool) bool {
	if !s.hasLyrics() {
		return true
	}
	return richRequested && !s.hasRich()
}

// skipSet computes which fan-out providers should not be consulted for this
// lookup. richRequested adds rich-only providers to the check. A provider is
// skipped when its ledger row is fresh (success is cached, miss is memoized)
// or, for video-keyed providers, when no video hint is available.
func (s *lyricsCacheState) skipSet(richRequested bool) map[string]bool {
	if s == nil {
		return nil
	}
	skip := make(map[string]bool, len(s.recentFetches))
	for provider := range s.recentFetches {
		skip[provider] = true
	}
	return skip
}

// resolveLyricsCacheState classifies the local cache for one lookup identity.// It performs exactly one FindTrackExact plus one ledger read; callers reuse
// the result instead of re-querying.
func resolveLyricsCacheState(ctx context.Context, metadataDB, lyricsDB *sql.DB, trackName, artistName, albumName string, duration float64, videoID string, misses *lyricsMissCache) (*lyricsCacheState, error) {
	state := &lyricsCacheState{
		richSources: make(map[string]struct{}),
		misses:      misses,
		missKey:     lyricsMissKeyWithVideo(trackName, artistName, albumName, videoID),
	}
	track, lyrics, err := db.FindTrackExact(ctx, metadataDB, lyricsDB, trackName, artistName, albumName, duration)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		state.track = track
		state.lyrics = lyrics
		if track.ID > 0 && lyricsDB != nil {
			if sources, listErr := db.ListRichLyricsSources(ctx, lyricsDB, track.ID); listErr == nil {
				state.richSources = sources
			}
			if fetches, fetchErr := db.ListRecentProviderFetches(ctx, lyricsDB, track.ID, providerFetchLedgerTTL); fetchErr == nil {
				state.recentFetches = fetches
			} else {
				state.recentFetches = make(map[string]bool)
			}
		}
	}
	if state.recentFetches == nil {
		state.recentFetches = make(map[string]bool)
	}
	return state, nil
}

// recordProviderFetch persists a fan-out outcome to the ledger so later
// requests skip the provider for the TTL window. It is fire-and-forget: a
// failed write never fails the request.
func recordProviderFetch(ctx context.Context, lyricsDB *sql.DB, trackID int64, provider string, success bool) {
	if lyricsDB == nil || trackID <= 0 {
		return
	}
	_ = db.UpsertProviderFetch(ctx, lyricsDB, trackID, provider, success)
}

// recordProviderMiss persists a fan-out miss to the ledger, but only for a
// complete identity (artist and album both known). The ledger is keyed by
// track alone, so a miss recorded under a partial identity (say title-only)
// would wrongly block a later retry with the artist/album backfilled from
// cached metadata. Incomplete-identity misses stay with the in-memory
// lyricsMisses cache, whose keys carry the full identity.
func recordProviderMiss(ctx context.Context, lyricsDB *sql.DB, trackID int64, provider, artistName, albumName string) {
	if strings.TrimSpace(artistName) == "" || strings.TrimSpace(albumName) == "" {
		return
	}
	recordProviderFetch(ctx, lyricsDB, trackID, provider, false)
}

// ledgerTTLFor returns the skip window for a provider. Misses use the same
// TTL as successes; the variadic form keeps call sites self-documenting.
func ledgerTTLFor() time.Duration { return providerFetchLedgerTTL }
