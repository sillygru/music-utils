package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/names"
	"github.com/sillygru/music-utils/internal/richlyrics"
	"github.com/sillygru/music-utils/internal/ttml"
)

const (
	defaultSearchLimit   = 20
	maxSearchLimit       = 50
	lyricsSearchCacheTTL = 24 * time.Hour
)

type fallbackBlockedError struct {
	status     int
	retryAfter int
}

func (e *fallbackBlockedError) Error() string { return "upstream fallback unavailable" }

type lyricsUpstreamCall struct {
	done   chan struct{}
	remote *lrclib.RemoteResult
	err    error
}

type lyricsUpstreamGroup struct {
	mu       sync.Mutex
	inFlight map[string]*lyricsUpstreamCall
}

func newLyricsUpstreamGroup() *lyricsUpstreamGroup {
	return &lyricsUpstreamGroup{inFlight: make(map[string]*lyricsUpstreamCall)}
}

// Do coalesces concurrent upstream lookups for the same exact lyrics key. The
// callback runs only for the leader, so only that request reserves fallback
// budget and enters the upstream queue.
func (g *lyricsUpstreamGroup) Do(ctx context.Context, key string, callback func() (*lrclib.RemoteResult, error)) (*lrclib.RemoteResult, error) {
	g.mu.Lock()
	if call, ok := g.inFlight[key]; ok {
		g.mu.Unlock()
		select {
		case <-call.done:
			return call.remote, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &lyricsUpstreamCall{done: make(chan struct{})}
	g.inFlight[key] = call
	g.mu.Unlock()

	call.remote, call.err = callback()
	g.mu.Lock()
	delete(g.inFlight, key)
	close(call.done)
	g.mu.Unlock()
	return call.remote, call.err
}

// lyricsResponse is the public lyrics response. Rich responses contain only
// RichSync; ordinary responses contain the available LRCLIB text fields. The
// legacy generated lyricsfile YAML payload is intentionally not exposed.
type lyricsResponse struct {
	ID           int64           `json:"id"`
	Name         string          `json:"name"`
	TrackName    string          `json:"trackName"`
	ArtistName   string          `json:"artistName"`
	AlbumName    string          `json:"albumName"`
	Duration     float64         `json:"duration"`
	Instrumental bool            `json:"instrumental"`
	PlainLyrics  string          `json:"plainLyrics,omitempty"`
	SyncedLyrics string          `json:"syncedLyrics,omitempty"`
	RichSync     *richSyncResult `json:"richSync,omitempty"`
	Variants     []lyricsVariant `json:"variants,omitempty"`
	Copyright    string          `json:"copyright,omitempty"`
}

type lyricsVariant struct {
	Provider     string          `json:"provider"`
	SyncType     string          `json:"syncType"`
	Format       string          `json:"format"`
	RichSync     *richSyncResult `json:"richSync,omitempty"`
	PlainLyrics  string          `json:"plainLyrics,omitempty"`
	SyncedLyrics string          `json:"syncedLyrics,omitempty"`
}

type richSyncResult struct {
	Content  any    `json:"content"`
	Format   string `json:"format"`
	SyncType string `json:"syncType"`
	Source   string `json:"source"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func getLyricsHandler(metadataDB, lyricsDB *sql.DB, providers *lyricsProviders, lyricsMisses *lyricsMissCache, fallbacks *fallbackGuard) http.HandlerFunc {
	lookupGroup := newLyricsLookupGroup()
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		candidates := names.Candidates(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"))
		input := candidates[0]
		trackName, artistName, albumName := input.TrackName, input.ArtistName, input.AlbumName
		if trackName == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "track_name is required"})
			return
		}
		// video_id is an optional hint forwarded only to video-keyed providers
		// (Zemer, YouTube official lyrics, YouTube subtitles). An invalid value
		// is ignored rather than rejected so older clients keep working.
		videoID := sanitizeVideoID(query.Get("video_id"))

		duration, err := optionalDuration(query.Get("duration"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "duration must be a non-negative number"})
			return
		}

		cacheStart := time.Now()
		state, stateErr := resolveLyricsCacheState(r.Context(), metadataDB, lyricsDB, trackName, artistName, albumName, duration, videoID, lyricsMisses)
		setCacheDuration(r, time.Since(cacheStart))
		if stateErr != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		existingTrack := state.track
		if state.hasLyrics() {
			setOutcome(r, "local_hit")
			// Rich gaps are filled synchronously only when the rich provider
			// was not already tried within the ledger TTL; a recently-tried
			// provider cannot have new data, so the cached response ships
			// immediately.
			if includeRichSync(r) && state.providerRecentlyTried("unison") {
				response := toLyricsResponse(state.track, state.lyrics)
				if state.track.ID > 0 && response.SyncedLyrics == "" {
					if cached, err := db.FindRichLyrics(r.Context(), lyricsDB, state.track.ID, ""); err == nil && !isWordRichEmpty(cached) {
						response.SyncedLyrics = compactRichSyncToLRC(cached)
					}
				}
				response.SyncedLyrics = ttml.CleanSyncedLyrics(response.SyncedLyrics)
				if response.PlainLyrics == "" && response.SyncedLyrics != "" {
					response.PlainLyrics = ttml.ExtractPlainFromLRC(response.SyncedLyrics)
				}
				syncType := requestedRichSyncType(r)
				if syncType != "" {
					if cached, err := db.FindRichLyrics(r.Context(), lyricsDB, state.track.ID, ""); err == nil && !isWordRichEmpty(cached) {
						setRichOnlyResponse(&response, cached)
					}
				}
				writeJSON(w, http.StatusOK, response)
				return
			}
			writeJSON(w, http.StatusOK, enrichLyricsResponse(r, state.track, state.lyrics, metadataDB, lyricsDB, providers, fallbacks))
			return
		}
		// A metadata row can exist before lyrics have been fetched. Treat an
		// empty, non-instrumental lyrics row as a cache miss so it cannot mask
		// a populated LRCLIB response for the same track.

		// resolveUpstream runs the full miss path for one identity: memoized-miss
		// check, rich-only attempt, then the parallel provider fan-out. The
		// identity is used exactly as given: callers pass strictly
		// user-provided values first and only retry with cached artist/album
		// when that fails. It reports whether the request was served; miss is
		// true only for a genuine miss (never for rate limiting or an
		// upstream-busy response, which are terminal).
		resolveUpstream := func(lookupTrack, lookupArtist, lookupAlbum string, existing *db.Track, cacheState *lyricsCacheState) (served, miss bool) {
			missKey := lyricsMissKeyWithVideo(lookupTrack, lookupArtist, lookupAlbum, videoID)
			lookupKey := missKey
			if lyricsMisses.Has(missKey, time.Now()) {
				if richResponse, ok := tryRichOnlyResponse(r, metadataDB, lyricsDB, providers.rich, fallbacks, providers.richEnabled, existing, lookupTrack, lookupArtist, lookupAlbum, duration); ok {
					setOutcome(r, "rich_lyrics_fallback_hit")
					writeJSON(w, http.StatusOK, richResponse)
					return true, false
				}
				return false, true
			}

			if !providers.anyEnabled(includeRichSync(r)) {
				if richResponse, ok := tryRichOnlyResponse(r, metadataDB, lyricsDB, providers.rich, fallbacks, providers.richEnabled, existing, lookupTrack, lookupArtist, lookupAlbum, duration); ok {
					setOutcome(r, "rich_lyrics_fallback_hit")
					writeJSON(w, http.StatusOK, richResponse)
					return true, false
				}
				return false, true
			}

			isrc := ""
			if existing != nil {
				isrc = existing.ISRC
			}
			skip := cacheState.skipSet(includeRichSync(r))
			parallelResult := lookupGroup.lookup(r.Context(), lookupKey, func(ctx context.Context, publish func(lyricsLookupResult)) {
				runParallelLyricsGet(ctx, publish, metadataDB, lyricsDB, providers, lyricsMisses, fallbacks, includeRichSync(r), clientIP(r, false), existing, lookupTrack, lookupArtist, lookupAlbum, duration, videoID, isrc, skip)
			})
			if parallelResult.upstream > 0 {
				setUpstreamDuration(r, parallelResult.upstream)
			}
			if parallelResult.status == http.StatusTooManyRequests {
				setOutcome(r, "rate_limited")
				writeRateLimitResponse(w, parallelResult.retry)
				return true, false
			}
			if parallelResult.status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", strconv.Itoa(parallelResult.retry))
				setOutcome(r, "upstream_busy")
				writeJSON(w, http.StatusServiceUnavailable, apiError{Code: http.StatusServiceUnavailable, Message: "Upstream busy, try again shortly"})
				return true, false
			}
			if response, ok := responseFromParallelLookup(parallelResult, includeRichSync(r)); ok {
				if parallelResult.rich != nil && includeRichSync(r) {
					setOutcome(r, "rich_lyrics_fallback_hit")
				} else {
					setOutcome(r, "lrclib_fallback_hit")
				}
				writeJSON(w, http.StatusOK, response)
				return true, false
			}
			return false, true
		}

		if served, _ := resolveUpstream(trackName, artistName, albumName, existingTrack, state); served {
			return
		}
		// Strict user-only lookup failed. Retry once with artist/album
		// backfilled from cached metadata (blanks only; user values win).
		// Duration is never backfilled and never sent upstream.
		fbArtist, fbAlbum, ok := fallbackLyricsIdentity(r.Context(), metadataDB, trackName, artistName, albumName, existingTrack)
		if !ok {
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
			return
		}
		setRequestIssue(r, slog.LevelInfo, "lyrics retry with cached artist/album")
		cacheStart = time.Now()
		fbState, fbStateErr := resolveLyricsCacheState(r.Context(), metadataDB, lyricsDB, trackName, fbArtist, fbAlbum, duration, videoID, lyricsMisses)
		setCacheDuration(r, time.Since(cacheStart))
		if fbStateErr != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		if fbState.hasLyrics() {
			setOutcome(r, "local_hit")
			writeJSON(w, http.StatusOK, enrichLyricsResponse(r, fbState.track, fbState.lyrics, metadataDB, lyricsDB, providers, fallbacks))
			return
		}
		fbExisting := existingTrack
		if fbState.track != nil {
			fbExisting = fbState.track
		}
		if served, _ := resolveUpstream(trackName, fbArtist, fbAlbum, fbExisting, fbState); served {
			return
		}
		setOutcome(r, "miss")
		writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
		return
	}
}

// fallbackLyricsIdentity backfills artist/album the user did not provide from
// cached metadata. It never overwrites user-provided values and never touches
// any other field (in particular duration). The exact-match metadata row is
// preferred when it carries the missing fields; otherwise the local catalog
// is searched by track name only. ok is true only when at least one blank
// field gained a value worth retrying.
func fallbackLyricsIdentity(ctx context.Context, metadataDB *sql.DB, trackName, artistName, albumName string, existing *db.Track) (fbArtist, fbAlbum string, ok bool) {
	fbArtist, fbAlbum = strings.TrimSpace(artistName), strings.TrimSpace(albumName)
	fill := func(artist, album string) {
		if fbArtist == "" {
			fbArtist = strings.TrimSpace(artist)
		}
		if fbAlbum == "" {
			fbAlbum = strings.TrimSpace(album)
		}
	}
	if existing != nil {
		fill(existing.ArtistName, existing.AlbumName)
	}
	if (fbArtist == "" || fbAlbum == "") && metadataDB != nil && strings.TrimSpace(trackName) != "" {
		if tracks, err := db.SearchTracks(ctx, metadataDB, nil, trackName, 1); err == nil {
			for i := range tracks {
				fill(tracks[i].Track.ArtistName, tracks[i].Track.AlbumName)
			}
		}
	}
	ok = fbArtist != strings.TrimSpace(artistName) || fbAlbum != strings.TrimSpace(albumName)
	return fbArtist, fbAlbum, ok
}

func searchLyricsHandlerWithUpstream(metadataDB, lyricsDB *sql.DB, client *lrclib.Client, richClient *richlyrics.Client, fallbacks *fallbackGuard, fallbackEnabled, richEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		searchQuery := names.CleanSearch(query.Get("q"))
		if searchQuery == "" {
			searchQuery = names.CleanSearch(strings.Join(nonEmpty(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name")), " "))
		}
		if searchQuery == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "q or track_name, artist_name, or album_name is required"})
			return
		}
		limit, err := searchLimit(query.Get("limit"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
			return
		}
		cacheKey := lyricsSearchCacheKey(searchQuery, limit, includeRichSync(r), requestedRichSyncType(r))
		if cached, cacheErr := db.FindLyricsSearchCache(r.Context(), lyricsDB, cacheKey, lyricsSearchCacheTTL); cacheErr == nil {
			var cachedResults []lyricsResponse
			if err := json.Unmarshal(cached, &cachedResults); err == nil {
				setOutcome(r, "local_hit")
				writeJSON(w, http.StatusOK, cachedResults)
				return
			}
		} else if !errors.Is(cacheErr, sql.ErrNoRows) {
			setRequestIssue(r, slog.LevelWarn, cacheErr.Error())
		}
		cacheStart := time.Now()
		tracks, err := db.SearchTracks(r.Context(), metadataDB, lyricsDB, searchQuery, limit)
		setCacheDuration(r, time.Since(cacheStart))
		if err != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		results := make([]lyricsResponse, 0, limit)
		seen := make(map[string]struct{}, limit)
		localIndexes := make(map[string]int, len(tracks))
		localTrackIDs := make(map[int64]struct{}, len(tracks))

		// Local rows are canonical: they can reuse cached rich variants and
		// should not be replaced by a duplicate LRCLIB release result. This is
		// especially important for rich search, where every uncached result
		// would otherwise require a separate paced provider request.
		for i := range tracks {
			key := strings.ToLower(strings.TrimSpace(tracks[i].Track.Name)) + "\x00" + strings.ToLower(strings.TrimSpace(tracks[i].Track.ArtistName)) + "\x00" + strings.ToLower(strings.TrimSpace(tracks[i].Track.AlbumName)) + "\x00" + strconv.FormatFloat(tracks[i].Track.Duration, 'f', 0, 64)
			if _, exists := seen[key]; exists || len(results) >= limit {
				continue
			}
			seen[key] = struct{}{}
			response := toLyricsResponse(&tracks[i].Track, &tracks[i].Lyrics)
			localIndexes[searchLyricsIdentity(response.TrackName, response.ArtistName)] = len(results)
			localTrackIDs[response.ID] = struct{}{}
			results = append(results, response)
		}

		appendResult := func(result lrclib.RemoteResult) {
			identity := searchLyricsIdentity(result.TrackName, result.ArtistName)
			if index, ok := localIndexes[identity]; ok {
				// Keep the local metadata/ID, but fill any missing LRC fields
				// from the matching upstream result before rich enrichment.
				mergeSearchLyrics(&results[index], result)
				return
			}
			key := strings.ToLower(strings.TrimSpace(result.TrackName)) + "\x00" + strings.ToLower(strings.TrimSpace(result.ArtistName)) + "\x00" + strings.ToLower(strings.TrimSpace(result.AlbumName)) + "\x00" + strconv.FormatFloat(result.Duration, 'f', 0, 64)
			if _, ok := seen[key]; ok || len(results) >= limit {
				return
			}
			seen[key] = struct{}{}
			track := db.Track{ID: result.ID, Name: result.TrackName, ArtistName: result.ArtistName, AlbumName: result.AlbumName, Duration: result.Duration, Source: "lrclib_fallback"}
			lyrics := db.Lyrics{PlainLyrics: result.PlainLyrics, SyncedLyrics: result.SyncedLyrics, Instrumental: result.Instrumental, Source: "lrclib_fallback"}
			trackID, _, persistErr := db.InsertTrackWithLyrics(r.Context(), metadataDB, lyricsDB, track, lyrics)
			if persistErr != nil {
				setRequestIssue(r, slog.LevelWarn, persistErr.Error())
			}
			if trackID > 0 {
				track.ID = trackID
				localTrackIDs[trackID] = struct{}{}
				localIndexes[identity] = len(results)
			}
			results = append(results, toLyricsResponse(&track, &lyrics))
		}
		// Rich-enabled searches are intended to serve the local rich cache when
		// a catalog result exists. Do not pay LRCLIB search latency just to
		// rediscover release variants that cannot improve the local response.
		localRichCacheHit := includeRichSync(r) && len(tracks) > 0
		if fallbackEnabled && client != nil && !localRichCacheHit {
			release, ok := fallbacks.enter(r, w)
			if !ok {
				return
			}
			upstreamStart := time.Now()
			remote, remoteErr := client.Search(r.Context(), searchQuery)
			setUpstreamDuration(r, time.Since(upstreamStart))
			release()
			if remoteErr == nil {
				for _, result := range remote {
					if synthesizedLyricsResult(result) {
						continue
					}
					appendResult(result)
				}
			}
		}

		// Rich enrichment happens after merging. Local tracks first consult the
		// persistent rich cache; only uncached remote-only rows call the provider.
		for i := range results {
			trackID := int64(0)
			cache := false
			if _, ok := localTrackIDs[results[i].ID]; ok {
				trackID = results[i].ID
				cache = true
			}
			enrichLyricsSearchResponse(r, lyricsDB, richClient, fallbacks, richEnabled, &results[i], trackID, cache, metadataDB)
		}
		sort.SliceStable(results, func(i, j int) bool {
			return searchResponseHasSync(results[i]) && !searchResponseHasSync(results[j])
		})
		if len(results) == 0 {
			setOutcome(r, "miss")
		} else if len(tracks) > 0 {
			setOutcome(r, "local_hit")
		} else {
			setOutcome(r, "lrclib_fallback_hit")
		}
		if encoded, encodeErr := json.Marshal(results); encodeErr == nil {
			if cacheErr := db.UpsertLyricsSearchCache(r.Context(), lyricsDB, cacheKey, encoded); cacheErr != nil {
				setRequestIssue(r, slog.LevelWarn, cacheErr.Error())
			}
		} else {
			setRequestIssue(r, slog.LevelWarn, encodeErr.Error())
		}
		writeJSON(w, http.StatusOK, results)
	}
}

func lyricsSearchCacheKey(query string, limit int, includeRich bool, syncType string) string {
	return lyricsCachePolicyVersion + "\x00search\x00" + canonicalPart(query) + "\x00" + strconv.Itoa(limit) + "\x00" + cacheBoolString(includeRich) + "\x00" + canonicalPart(syncType)
}

func searchLyricsIdentity(trackName, artistName string) string {
	return strings.ToLower(strings.TrimSpace(trackName)) + "\x00" + strings.ToLower(strings.TrimSpace(artistName))
}

func mergeSearchLyrics(response *lyricsResponse, result lrclib.RemoteResult) {
	if response == nil {
		return
	}
	if response.PlainLyrics == "" {
		response.PlainLyrics = result.PlainLyrics
	}
	if response.SyncedLyrics == "" {
		response.SyncedLyrics = result.SyncedLyrics
	}
	if result.Instrumental {
		response.Instrumental = true
	}
}

func searchResponseHasSync(response lyricsResponse) bool {
	return response.RichSync != nil || response.SyncedLyrics != ""
}

func enrichLyricsSearchResponse(r *http.Request, lyricsDB *sql.DB, client *richlyrics.Client, fallbacks *fallbackGuard, enabled bool, response *lyricsResponse, trackID int64, cache bool, metadataDB ...*sql.DB) {
	if !enabled || client == nil || !includeRichSync(r) || response == nil {
		return
	}
	syncType := requestedRichSyncType(r)
	var mdb *sql.DB
	if len(metadataDB) > 0 {
		mdb = metadataDB[0]
	}
	if cache && trackID > 0 {
		if cached, err := db.FindRichLyrics(r.Context(), lyricsDB, trackID, syncType); err == nil {
			if !isWordRichEmpty(cached) {
				setRichOnlyResponse(response, cached)
				return
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			setRequestIssue(r, slog.LevelWarn, err.Error())
			return
		} else if mdb != nil && response.ArtistName != "" {
			if byName, err := db.FindRichLyricsByName(r.Context(), mdb, lyricsDB, response.TrackName, response.ArtistName, syncType); err == nil && !isWordRichEmpty(byName) {
				setRichOnlyResponse(response, byName)
				return
			}
		}
		if syncType != "" && mdb != nil && response.ArtistName != "" {
			if byName, err := db.FindRichLyricsByName(r.Context(), mdb, lyricsDB, response.TrackName, response.ArtistName, ""); err == nil && !isWordRichEmpty(byName) {
				setRichOnlyResponse(response, byName)
				return
			}
		}
	}
	if fallbacks == nil {
		return
	}
	release, _, _, ok := fallbacks.acquire(r)
	if !ok {
		return
	}
	defer release()
	started := time.Now()
	remote, err := client.Get(r.Context(), response.TrackName, response.ArtistName, response.AlbumName)
	setUpstreamDuration(r, time.Since(started))
	if err != nil {
		if !errors.Is(err, richlyrics.ErrNotFound) {
			setRequestIssue(r, slog.LevelWarn, err.Error())
		}
		return
	}
	if !validRichSyncType(remote.SyncType) {

		setRequestIssue(r, slog.LevelWarn, "rich lyrics returned unsupported sync type")
		return
	}
	content, format, converted := compactRichSyncForStorage(remote.Content, remote.Format)
	if !converted {
		content, format = remote.Content, remote.Format
	}
	rich := db.RichLyrics{TrackID: trackID, Content: content, Format: format, SyncType: remote.SyncType, Source: remote.Source}
	if isWordRichEmpty(&rich) {
		return
	}
	if cache && trackID > 0 {
		if err := db.UpsertRichLyrics(r.Context(), lyricsDB, rich); err != nil {
			setRequestIssue(r, slog.LevelWarn, err.Error())
			return
		}
	}
	setRichOnlyResponse(response, &rich)
}

func searchLyricsHandler(metadataDB, lyricsDB *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		searchQuery := names.CleanSearch(query.Get("q"))
		if searchQuery == "" {
			searchQuery = names.CleanSearch(strings.Join(nonEmpty(
				query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"),
			), " "))
		}
		if searchQuery == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "q or track_name, artist_name, or album_name is required"})
			return
		}

		limit, err := searchLimit(query.Get("limit"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
			return
		}

		cacheStart := time.Now()
		tracks, err := db.SearchTracks(r.Context(), metadataDB, lyricsDB, searchQuery, limit)
		setCacheDuration(r, time.Since(cacheStart))
		if err != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}

		results := make([]lyricsResponse, 0, len(tracks))
		for i := range tracks {
			results = append(results, toLyricsResponse(&tracks[i].Track, &tracks[i].Lyrics))
		}
		if len(results) == 0 {
			setOutcome(r, "miss")
		} else {
			setOutcome(r, "local_hit")
		}
		writeJSON(w, http.StatusOK, results)
	}
}

func remoteLyricsAvailable(result *lrclib.RemoteResult) bool {
	if result == nil {
		return false
	}
	return result.Instrumental || result.PlainLyrics != "" || result.SyncedLyrics != ""
}

// remoteLyricsMatchesInput validates the identity returned by LRCLIB before
// its content is cached. Album names are allowed to differ because LRCLIB may
// resolve the same recording on a different release, but track and artist must
// match the requested candidate.
func remoteLyricsMatchesInput(input names.Input, result *lrclib.RemoteResult) bool {
	if result == nil {
		return false
	}
	actual := names.Normalize(result.TrackName, result.ArtistName, result.AlbumName)
	if strings.ToLower(strings.TrimSpace(actual.TrackName)) != strings.ToLower(strings.TrimSpace(input.TrackName)) {
		return false
	}
	if strings.TrimSpace(input.ArtistName) != "" &&
		strings.ToLower(strings.TrimSpace(actual.ArtistName)) != strings.ToLower(strings.TrimSpace(input.ArtistName)) {
		return false
	}
	return true
}

// lookupRemoteLyrics resolves lyrics upstream. Only title, artist, and album
// are ever sent: duration and all other metadata are deliberately excluded so
// a duration mismatch can never filter out the correct recording. Duration is
// used only to rank candidates locally. LRCLIB's exact endpoint requires an
// artist, so an artist-less request resolves through search and selects the
// best result instead.
func lookupRemoteLyrics(ctx context.Context, client *lrclib.Client, trackName, artistName, albumName string) (*lrclib.RemoteResult, error) {
	return lookupRemoteLyricsWithDuration(ctx, client, trackName, artistName, albumName, 0)
}

// lookupRemoteLyricsWithDuration is lookupRemoteLyrics plus the local duration
// hint used by the multi-strategy fallback chain (±2s strict, ±5s relaxed).
func lookupRemoteLyricsWithDuration(ctx context.Context, client *lrclib.Client, trackName, artistName, albumName string, duration float64) (*lrclib.RemoteResult, error) {
	var lastErr error
	for _, candidate := range names.Candidates(trackName, artistName, albumName) {
		if candidate.ArtistName == "" {
			searchResults, err := client.Search(ctx, strings.Join(nonEmpty(candidate.TrackName, candidate.AlbumName), " "))
			if err != nil {
				lastErr = err
				continue
			}
			if remote := matchLyricsByName(searchResults, candidate.TrackName, candidate.AlbumName); remote != nil {
				return remote, nil
			}
			lastErr = lrclib.ErrNotFound
			continue
		}
		remote, err := client.GetExact(ctx, candidate.TrackName, candidate.ArtistName, candidate.AlbumName)
		if err == nil {
			if remoteLyricsMatchesInput(candidate, remote) && remoteLyricsAvailable(remote) {
				return remote, nil
			}
			// A successful HTTP response is not necessarily the requested
			// recording. Fall through to the multi-strategy search before
			// treating this as a miss.
			err = lrclib.ErrNotFound
		}
		if err != nil && !errors.Is(err, lrclib.ErrNotFound) {
			lastErr = err
			continue
		}
		if fallback, fallbackErr := client.GetWithFallbacks(ctx, candidate.TrackName, candidate.ArtistName, candidate.AlbumName, duration); fallbackErr == nil {
			if remoteLyricsMatchesInput(candidate, fallback) && remoteLyricsAvailable(fallback) {
				return fallback, nil
			}
			err = lrclib.ErrNotFound
		} else if !errors.Is(fallbackErr, lrclib.ErrNotFound) {
			lastErr = fallbackErr
			continue
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = lrclib.ErrNotFound
	}
	return nil, lastErr
}

// matchLyricsByName selects the best search result for an artist-less request:
// a result whose track name contains the requested track, preferring one that
// also matches the album hint. Synthesized rows (where LRCLIB fills every
// field with the query) are never selected.
func matchLyricsByName(results []lrclib.RemoteResult, trackName, albumName string) *lrclib.RemoteResult {
	var best *lrclib.RemoteResult
	for i := range results {
		result := &results[i]
		if synthesizedLyricsResult(*result) || !remoteLyricsAvailable(result) {
			continue
		}
		if !lyricsNameContains(result.TrackName, trackName) {
			continue
		}
		if best == nil {
			best = result
		}
		if albumName != "" && lyricsNameContains(result.AlbumName, albumName) {
			return result
		}
	}
	return best
}

// lyricsNameContains reports whether candidate contains want after
// normalization, requiring every requested token to appear as a whole token or
// a prefix of one. This matches "radiohead - creep" for "creep" without
// matching "creeper".
func lyricsNameContains(candidate, want string) bool {
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	want = strings.ToLower(strings.TrimSpace(want))
	if candidate == "" || want == "" {
		return false
	}
	candidateTokens := strings.Fields(candidate)
	for _, token := range strings.Fields(want) {
		found := false
		for _, candidateToken := range candidateTokens {
			if candidateToken == token || strings.HasPrefix(candidateToken, token) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// synthesizedLyricsResult reports whether LRCLIB filled every identity field
// with the same value, which is the signature of a synthesized placeholder row
// whose metadata cannot be trusted. Rows without a track name are treated the
// same way.
func synthesizedLyricsResult(result lrclib.RemoteResult) bool {
	track := strings.ToLower(strings.TrimSpace(result.TrackName))
	if track == "" {
		return true
	}
	return track == strings.ToLower(strings.TrimSpace(result.ArtistName)) &&
		track == strings.ToLower(strings.TrimSpace(result.AlbumName))
}

func matchingLyricsResult(results []lrclib.RemoteResult, trackName, artistName string) *lrclib.RemoteResult {
	for i := range results {
		result := &results[i]
		if strings.EqualFold(strings.TrimSpace(result.TrackName), strings.TrimSpace(trackName)) &&
			strings.EqualFold(strings.TrimSpace(result.ArtistName), strings.TrimSpace(artistName)) &&
			remoteLyricsAvailable(result) {
			return result
		}
	}
	return nil
}

func tryRichOnlyResponse(r *http.Request, metadataDB, lyricsDB *sql.DB, client *richlyrics.Client, fallbacks *fallbackGuard, enabled bool, existingTrack *db.Track, trackName, artistName, albumName string, duration float64) (lyricsResponse, bool) {
	if !enabled || client == nil || !includeRichSync(r) {
		return lyricsResponse{}, false
	}
	if existingTrack != nil && existingTrack.ID > 0 {
		if cached, err := db.FindRichLyrics(r.Context(), lyricsDB, existingTrack.ID, requestedRichSyncType(r)); err == nil {
			response := toLyricsResponse(existingTrack, &db.Lyrics{})
			setRichOnlyResponse(&response, cached)
			return response, true
		}
	}
	if fallbacks == nil {
		return lyricsResponse{}, false
	}
	release, _, _, ok := fallbacks.acquire(r)
	if !ok {
		return lyricsResponse{}, false
	}
	defer release()
	started := time.Now()
	// Strictly what the caller passed: no auto-fill from the cached metadata
	// row here. The getLyricsHandler retries with cached artist/album only
	// after this strict attempt misses, and duration is never sent upstream.
	remote, err := client.Get(r.Context(), strings.TrimSpace(trackName), strings.TrimSpace(artistName), strings.TrimSpace(albumName))
	setUpstreamDuration(r, time.Since(started))
	if err != nil {
		if !errors.Is(err, richlyrics.ErrNotFound) {
			setRequestIssue(r, slog.LevelWarn, err.Error())
		}
		return lyricsResponse{}, false
	}
	if !validRichSyncType(remote.SyncType) {

		return lyricsResponse{}, false
	}
	track := db.Track{
		Name:       strings.TrimSpace(trackName),
		ArtistName: artistName,
		AlbumName:  albumName,
		Duration:   duration,
		Source:     "unison_rich_fallback",
	}
	if existingTrack != nil {
		track = *existingTrack
		track.Source = "unison_rich_fallback"
	}
	if remoteTrackName := strings.TrimSpace(track.Name); remoteTrackName == "" {
		return lyricsResponse{}, false
	}
	trackID, _, err := db.InsertTrackWithLyrics(r.Context(), metadataDB, lyricsDB, track, db.Lyrics{Source: "unison_rich_fallback"})
	if err != nil {
		setRequestIssue(r, slog.LevelWarn, err.Error())
		return lyricsResponse{}, false
	}
	track.ID = trackID
	content, format, converted := compactRichSyncForStorage(remote.Content, remote.Format)
	if !converted {
		content, format = remote.Content, remote.Format
	}
	rich := db.RichLyrics{TrackID: trackID, Content: content, Format: format, SyncType: remote.SyncType, Source: remote.Source}
	if isWordRichEmpty(&rich) {
		return lyricsResponse{}, false
	}
	if err := db.UpsertRichLyrics(r.Context(), lyricsDB, rich); err != nil {
		setRequestIssue(r, slog.LevelWarn, err.Error())
		return lyricsResponse{}, false
	}
	response := toLyricsResponse(&track, &db.Lyrics{})
	setRichOnlyResponse(&response, &rich)
	return response, true
}

func enrichLyricsResponse(r *http.Request, track *db.Track, lyrics *db.Lyrics, metadataDB, lyricsDB *sql.DB, providers *lyricsProviders, fallbacks *fallbackGuard) lyricsResponse {
	client := providers.rich
	enabled := providers.richEnabled
	return enrichLyricsResponseWithClient(r, track, lyrics, metadataDB, lyricsDB, client, fallbacks, enabled, providers)
}

func enrichLyricsResponseWithClient(r *http.Request, track *db.Track, lyrics *db.Lyrics, metadataDB, lyricsDB *sql.DB, client *richlyrics.Client, fallbacks *fallbackGuard, enabled bool, providers *lyricsProviders) lyricsResponse {
	response := toLyricsResponse(track, lyrics)
	if track != nil && track.ID > 0 && response.SyncedLyrics == "" {
		if cached, err := db.FindRichLyrics(r.Context(), lyricsDB, track.ID, ""); err == nil && !isWordRichEmpty(cached) {
			response.SyncedLyrics = compactRichSyncToLRC(cached)
		} else if metadataDB != nil && track.ArtistName != "" {
			if byName, err2 := db.FindRichLyricsByName(r.Context(), metadataDB, lyricsDB, track.Name, track.ArtistName, ""); err2 == nil && !isWordRichEmpty(byName) {
				response.SyncedLyrics = compactRichSyncToLRC(byName)
			}
		}
	}
	response.SyncedLyrics = ttml.CleanSyncedLyrics(response.SyncedLyrics)
	if response.PlainLyrics == "" && response.SyncedLyrics != "" {
		response.PlainLyrics = ttml.ExtractPlainFromLRC(response.SyncedLyrics)
	}
	if !enabled || client == nil || !includeRichSync(r) || track == nil || track.ID <= 0 {
		return response
	}
	syncType := requestedRichSyncType(r)
	if cached, err := db.FindRichLyrics(r.Context(), lyricsDB, track.ID, syncType); err == nil {
		if !isWordRichEmpty(cached) {
			setRichOnlyResponse(&response, cached)
			return response
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		setRequestIssue(r, slog.LevelWarn, err.Error())
		return response
	} else {
		if metadataDB != nil && track.ArtistName != "" {
			if byName, err2 := db.FindRichLyricsByName(r.Context(), metadataDB, lyricsDB, track.Name, track.ArtistName, syncType); err2 == nil && !isWordRichEmpty(byName) {
				setRichOnlyResponse(&response, byName)
				return response
			}
		}
		if syncType != "" {
			if cached2, err2 := db.FindRichLyrics(r.Context(), lyricsDB, track.ID, ""); err2 == nil && !isWordRichEmpty(cached2) {
				setRichOnlyResponse(&response, cached2)
				return response
			}
			if metadataDB != nil && track.ArtistName != "" {
				if byName2, err3 := db.FindRichLyricsByName(r.Context(), metadataDB, lyricsDB, track.Name, track.ArtistName, ""); err3 == nil && !isWordRichEmpty(byName2) {
					setRichOnlyResponse(&response, byName2)
					return response
				}
			}
		}
	}
	if fallbacks == nil {
		return response
	}
	release, _, _, ok := fallbacks.acquire(r)
	if !ok {
		return response
	}
	defer release()
	started := time.Now()
	query := r.URL.Query()
	candidates := names.Candidates(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"))
	input := candidates[0]
	lookupTrack := input.TrackName
	if lookupTrack == "" {
		lookupTrack = track.Name
	}

	type richCandidate struct {
		rich     *db.RichLyrics
		priority int
	}
	var best richCandidate
	var mu sync.Mutex
	var wg sync.WaitGroup

	updateBest := func(c richCandidate) {
		mu.Lock()
		defer mu.Unlock()
		if c.rich == nil || isWordRichEmpty(c.rich) {
			return
		}
		if best.rich == nil || c.priority > best.priority {
			best = c
		}
	}

	// Unison in parallel
	if enabled && client != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote, err := client.Get(r.Context(), lookupTrack, input.ArtistName, input.AlbumName)
			if err != nil && track != nil &&
				(strings.TrimSpace(input.ArtistName) == "" || strings.TrimSpace(input.AlbumName) == "") {
				fbArtist, fbAlbum := strings.TrimSpace(input.ArtistName), strings.TrimSpace(input.AlbumName)
				if fbArtist == "" {
					fbArtist = strings.TrimSpace(track.ArtistName)
				}
				if fbAlbum == "" {
					fbAlbum = strings.TrimSpace(track.AlbumName)
				}
				if fbArtist != strings.TrimSpace(input.ArtistName) || fbAlbum != strings.TrimSpace(input.AlbumName) {
					remote, err = client.Get(r.Context(), lookupTrack, fbArtist, fbAlbum)
				}
			}
			if err != nil || remote == nil || !validRichSyncType(remote.SyncType) {
				return
			}
			content, format, converted := compactRichSyncForStorage(remote.Content, remote.Format)
			if !converted {
				content, format = remote.Content, remote.Format
			}
			cached := db.RichLyrics{TrackID: track.ID, Content: content, Format: format, SyncType: remote.SyncType, Source: remote.Source}
			if !isWordRichEmpty(&cached) {
				_ = db.UpsertRichLyrics(r.Context(), lyricsDB, cached)
				updateBest(richCandidate{rich: &cached, priority: 2})
			}
		}()
	}

	// LyricsPlus in parallel (CONCURRENT, NOT SEQUENTIAL)
	if providers != nil && providers.lyricsPlusEnabled && providers.lyricsPlus != nil && track != nil && track.ID > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lpRemote, lpErr := providers.lyricsPlus.Get(r.Context(), lookupTrack, input.ArtistName, input.AlbumName, track.Duration, track.ISRC)
			if lpErr != nil || lpRemote == nil || !lpRemote.WordSynced {
				return
			}
			var lpRich db.RichLyrics
			if strings.TrimSpace(lpRemote.TTML) != "" {
				c, f, conv := compactRichSyncForStorage(lpRemote.TTML, "ttml")
				if !conv {
					c, f = lpRemote.TTML, "ttml"
				}
				lpRich = db.RichLyrics{TrackID: track.ID, Content: c, Format: f, SyncType: "word", Source: "lyricsplus"}
			} else if strings.TrimSpace(lpRemote.RichJSON) != "" {
				lpRich = db.RichLyrics{TrackID: track.ID, Content: lpRemote.RichJSON, Format: "json", SyncType: "word", Source: "lyricsplus"}
			}
			if lpRich.Content != "" && !isWordRichEmpty(&lpRich) {
				_ = db.UpsertRichLyrics(r.Context(), lyricsDB, lpRich)
				if stored, err3 := db.FindRichLyrics(r.Context(), lyricsDB, track.ID, "word"); err3 == nil && !isWordRichEmpty(stored) {
					updateBest(richCandidate{rich: stored, priority: 3})
				} else {
					updateBest(richCandidate{rich: &lpRich, priority: 3})
				}
			}
		}()
	}

	wg.Wait()
	setUpstreamDuration(r, time.Since(started))
	if best.rich != nil {
		setRichOnlyResponse(&response, best.rich)
	}
	return response
}

func includeRichSync(r *http.Request) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("include_rich_sync")))
	return value == "1" || value == "true" || value == "yes"
}

func requestedRichSyncType(r *http.Request) string {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sync_type")))
	if validRichSyncType(value) {
		return value
	}
	return ""
}

func validRichSyncType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "word", "syllable", "richsync":
		return true
	default:
		return false
	}
}

func setRichOnlyResponse(response *lyricsResponse, rich *db.RichLyrics) {
	if response == nil || rich == nil || isWordRichEmpty(rich) {
		return
	}
	content := compactRichSyncContent(rich.Content, rich.Format)
	if content == nil {
		return
	}
	format := rich.Format
	if _, ok := content.(compactRichSync); ok {
		format = "json"
	}
	response.RichSync = &richSyncResult{Content: content, Format: format, SyncType: rich.SyncType, Source: rich.Source}
	if strings.TrimSpace(response.SyncedLyrics) == "" {
		response.SyncedLyrics = compactRichSyncToLRC(rich)
	}
	response.SyncedLyrics = ttml.CleanSyncedLyrics(response.SyncedLyrics)
	if strings.TrimSpace(response.PlainLyrics) == "" {
		if crs, ok := content.(compactRichSync); ok && len(crs.Lines) > 0 {
			var b strings.Builder
			for _, l := range crs.Lines {
				txt := strings.TrimSpace(l.Text)
				if txt != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(txt)
				}
			}
			response.PlainLyrics = b.String()
		}
	}
	if strings.TrimSpace(response.PlainLyrics) == "" && strings.TrimSpace(response.SyncedLyrics) != "" {
		response.PlainLyrics = ttml.ExtractPlainFromLRC(response.SyncedLyrics)
	}
}

func lyricsAvailable(lyrics *db.Lyrics) bool {
	if lyrics == nil {
		return false
	}
	return lyrics.Instrumental || lyrics.PlainLyrics != "" || lyrics.SyncedLyrics != ""
}

func toLyricsResponse(track *db.Track, lyrics *db.Lyrics) lyricsResponse {
	response := lyricsResponse{
		ID:         track.ID,
		Name:       track.Name,
		TrackName:  track.Name,
		ArtistName: track.ArtistName,
		AlbumName:  track.AlbumName,
		Duration:   track.Duration,
	}
	if lyrics != nil {
		response.Instrumental = lyrics.Instrumental
		response.PlainLyrics = lyrics.PlainLyrics
		response.SyncedLyrics = ttml.CleanSyncedLyrics(lyrics.SyncedLyrics)
		if response.PlainLyrics == "" && response.SyncedLyrics != "" {
			response.PlainLyrics = ttml.ExtractPlainFromLRC(response.SyncedLyrics)
		}
	}
	return response
}

func searchLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultSearchLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxSearchLimit {
		return 0, errors.New("invalid limit")
	}
	return limit, nil
}

func nonEmpty(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
