package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/names"
)

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 50
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

// lyricsResponse mirrors the object shape LRCLIB returns for lyrics lookups
// and search results. Name and LyricsFile are derived from the stored row at
// serialization time (name always equals the track name; lyricsfile is the
// generated YAML payload), so the API stays drop-in compatible with LRCLIB
// clients without storing extra data.
type lyricsResponse struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
	LyricsFile   string  `json:"lyricsfile"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func getLyricsHandler(metadataDB, lyricsDB *sql.DB, client *lrclib.Client, lyricsMisses *lyricsMissCache, fallbacks *fallbackGuard, fallbackEnabled bool, prefetcher *prefetcher) http.HandlerFunc {
	upstreamGroup := newLyricsUpstreamGroup()
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

		duration, err := optionalDuration(query.Get("duration"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "duration must be a non-negative number"})
			return
		}

		cacheStart := time.Now()
		var track *db.Track
		var lyrics *db.Lyrics
		for _, candidate := range candidates {
			track, lyrics, err = db.FindTrackExact(
				r.Context(), metadataDB, lyricsDB, candidate.TrackName, candidate.ArtistName, candidate.AlbumName, duration,
			)
			if err == nil || !errors.Is(err, sql.ErrNoRows) {
				break
			}
		}
		setCacheDuration(r, time.Since(cacheStart))
		existingTrack := track
		if err == nil && lyricsAvailable(lyrics) {
			setOutcome(r, "local_hit")
			prefetcher.Enqueue(track.Name, track.ArtistName, track.AlbumName, track.Duration)
			writeJSON(w, http.StatusOK, toLyricsResponse(track, lyrics))
			return
		}
		// A metadata row can exist before lyrics have been fetched. Treat an
		// empty, non-instrumental lyrics row as a cache miss so it cannot mask
		// a populated LRCLIB response for the same track.
		if err == nil {
			err = sql.ErrNoRows
		}
		if !errors.Is(err, sql.ErrNoRows) {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}

		missKey := lyricsMissKey(trackName, artistName, albumName, duration)
		if lyricsMisses.Has(missKey, time.Now()) {
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
			return
		}

		if !fallbackEnabled || client == nil {
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
			return
		}

		remote, remoteErr := upstreamGroup.Do(r.Context(), missKey, func() (*lrclib.RemoteResult, error) {
			release, status, retryAfter, ok := fallbacks.acquire(r)
			if !ok {
				return nil, &fallbackBlockedError{status: status, retryAfter: retryAfter}
			}
			defer release()

			upstreamStart := time.Now()
			remote, err := lookupRemoteLyrics(r.Context(), client, query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"), duration)
			setUpstreamDuration(r, time.Since(upstreamStart))
			return remote, err
		})
		var blocked *fallbackBlockedError
		if errors.As(remoteErr, &blocked) {
			if blocked.status == http.StatusTooManyRequests {
				setOutcome(r, "rate_limited")
				writeRateLimitResponse(w, blocked.retryAfter)
			} else {
				w.Header().Set("Retry-After", strconv.Itoa(blocked.retryAfter))
				setOutcome(r, "upstream_busy")
				writeJSON(w, http.StatusServiceUnavailable, apiError{Code: http.StatusServiceUnavailable, Message: "Upstream busy, try again shortly"})
			}
			return
		}
		if remoteErr != nil && !errors.Is(remoteErr, lrclib.ErrNotFound) {
			setRequestIssue(r, slog.LevelWarn, remoteErr.Error())
		}
		if remoteErr == nil && !remoteLyricsAvailable(remote) {
			// LRCLIB can return a successful metadata-only record for a release
			// variant. It is not a usable lyrics hit, so broaden the lookup below.
			remoteErr = lrclib.ErrNotFound
		}
		if remoteErr != nil {
			// LRCLIB's exact endpoint is release-sensitive when an album hint is
			// supplied. Broaden that lookup through search by track and artist;
			// leave album-less 404s as genuine misses to preserve the fallback
			// budget and avoid an unnecessary second upstream request.
			if artistName != "" && errors.Is(remoteErr, lrclib.ErrNotFound) && albumName != "" {
				searchStart := time.Now()
				searchResults, searchErr := client.Search(r.Context(), strings.Join(nonEmpty(trackName, artistName), " "))
				setUpstreamDuration(r, time.Since(searchStart))
				if searchErr == nil {
					remote = matchingLyricsResult(searchResults, trackName, artistName)
					if remote != nil {
						remoteErr = nil
					}
				} else {
					setRequestIssue(r, slog.LevelWarn, searchErr.Error())
				}
			}
			if remoteErr != nil {
				lyricsMisses.Set(missKey, time.Now())
				setOutcome(r, "miss")
				writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
				return
			}
		}

		cachedTrack := db.Track{
			Name:       firstNonEmpty(remote.TrackName, trackName),
			ArtistName: firstNonEmpty(remote.ArtistName, artistName),
			AlbumName:  firstNonEmpty(remote.AlbumName, albumName),
			Duration:   remote.Duration,
			Source:     "lrclib_fallback",
		}
		if cachedTrack.Duration <= 0 {
			cachedTrack.Duration = duration
		}
		cacheTrack := cachedTrack
		if existingTrack != nil {
			// Refresh the already-known metadata row in place so a request that
			// supplied a release alias becomes a local hit next time.
			cacheTrack = *existingTrack
			cacheTrack.Source = "lrclib_fallback"
		}
		trackID, _, err := db.InsertTrackWithLyrics(r.Context(), metadataDB, lyricsDB, cacheTrack, db.Lyrics{
			PlainLyrics:  remote.PlainLyrics,
			SyncedLyrics: remote.SyncedLyrics,
			Instrumental: remote.Instrumental,
			Source:       "lrclib_fallback",
		})
		if err != nil {
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}

		cacheTrack.ID = trackID
		track = &cacheTrack
		lyrics = &db.Lyrics{
			PlainLyrics:  remote.PlainLyrics,
			SyncedLyrics: remote.SyncedLyrics,
			Instrumental: remote.Instrumental,
		}
		setOutcome(r, "lrclib_fallback_hit")
		prefetcher.Enqueue(cacheTrack.Name, cacheTrack.ArtistName, cacheTrack.AlbumName, cacheTrack.Duration)
		writeJSON(w, http.StatusOK, toLyricsResponse(track, lyrics))
	}
}

func searchLyricsHandlerWithUpstream(metadataDB, lyricsDB *sql.DB, client *lrclib.Client, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
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
		appendResult := func(result lrclib.RemoteResult) {
			key := strings.ToLower(strings.TrimSpace(result.TrackName)) + "\x00" + strings.ToLower(strings.TrimSpace(result.ArtistName)) + "\x00" + strings.ToLower(strings.TrimSpace(result.AlbumName)) + "\x00" + strconv.FormatFloat(result.Duration, 'f', 0, 64)
			if _, ok := seen[key]; ok || len(results) >= limit {
				return
			}
			seen[key] = struct{}{}
			track := &db.Track{ID: result.ID, Name: result.TrackName, ArtistName: result.ArtistName, AlbumName: result.AlbumName, Duration: result.Duration}
			lyrics := &db.Lyrics{PlainLyrics: result.PlainLyrics, SyncedLyrics: result.SyncedLyrics, Instrumental: result.Instrumental}
			results = append(results, toLyricsResponse(track, lyrics))
		}
		// Put LRCLIB results first so a warm local catalog cannot hide the
		// upstream search merely because it fills the final limit.
		if fallbackEnabled && client != nil {
			release, ok := fallbacks.enter(r, w)
			if !ok {
				return
			}
			defer release()
			upstreamStart := time.Now()
			remote, remoteErr := client.Search(r.Context(), searchQuery)
			setUpstreamDuration(r, time.Since(upstreamStart))
			if remoteErr == nil {
				for _, result := range remote {
					if synthesizedLyricsResult(result) {
						continue
					}
					appendResult(result)
				}
			}
		}
		for i := range tracks {
			key := strings.ToLower(strings.TrimSpace(tracks[i].Track.Name)) + "\x00" + strings.ToLower(strings.TrimSpace(tracks[i].Track.ArtistName)) + "\x00" + strings.ToLower(strings.TrimSpace(tracks[i].Track.AlbumName)) + "\x00" + strconv.FormatFloat(tracks[i].Track.Duration, 'f', 0, 64)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			if len(results) < limit {
				results = append(results, toLyricsResponse(&tracks[i].Track, &tracks[i].Lyrics))
			}
		}
		if len(results) == 0 {
			setOutcome(r, "miss")
		} else if len(tracks) > 0 {
			setOutcome(r, "local_hit")
		} else {
			setOutcome(r, "lrclib_fallback_hit")
		}
		writeJSON(w, http.StatusOK, results)
	}
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

// lookupRemoteLyrics resolves lyrics upstream. LRCLIB's exact endpoint requires
// an artist, so an artist-less request resolves through search and selects the
// best result instead.
func lookupRemoteLyrics(ctx context.Context, client *lrclib.Client, trackName, artistName, albumName string, duration float64) (*lrclib.RemoteResult, error) {
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
		remote, err := client.GetExact(ctx, candidate.TrackName, candidate.ArtistName, candidate.AlbumName, duration)
		if err == nil {
			if remoteLyricsMatchesInput(candidate, remote) && remoteLyricsAvailable(remote) {
				return remote, nil
			}
			// A successful HTTP response is not necessarily the requested
			// recording. Treat an identity mismatch like a miss so a different
			// song can never be persisted under this request's cache key.
			err = lrclib.ErrNotFound
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

func lyricsAvailable(lyrics *db.Lyrics) bool {
	if lyrics == nil {
		return false
	}
	return lyrics.Instrumental || lyrics.PlainLyrics != "" || lyrics.SyncedLyrics != ""
}

func toLyricsResponse(track *db.Track, lyrics *db.Lyrics) lyricsResponse {
	return lyricsResponse{
		ID:           track.ID,
		Name:         track.Name,
		TrackName:    track.Name,
		ArtistName:   track.ArtistName,
		AlbumName:    track.AlbumName,
		Duration:     track.Duration,
		Instrumental: lyrics.Instrumental,
		PlainLyrics:  lyrics.PlainLyrics,
		SyncedLyrics: lyrics.SyncedLyrics,
		LyricsFile:   buildLyricsFile(track, lyrics),
	}
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
