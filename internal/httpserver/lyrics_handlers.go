package httpserver

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
)

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 50
)

type lyricsResponse struct {
	ID           int64   `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func getLyricsHandler(metadataDB, lyricsDB *sql.DB, client *lrclib.Client, lyricsMisses *lyricsMissCache, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		trackName := strings.TrimSpace(query.Get("track_name"))
		artistName := strings.TrimSpace(query.Get("artist_name"))
		if trackName == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "track_name is required"})
			return
		}
		if artistName == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "artist_name is required"})
			return
		}

		duration, err := optionalDuration(query.Get("duration"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "duration must be a non-negative number"})
			return
		}

		cacheStart := time.Now()
		track, lyrics, err := db.FindTrackExact(
			r.Context(), metadataDB, lyricsDB, trackName, artistName, query.Get("album_name"), duration,
		)
		setCacheDuration(r, time.Since(cacheStart))
		existingTrack := track
		if err == nil && lyricsAvailable(lyrics) {
			setOutcome(r, "local_hit")
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

		missKey := lyricsMissKey(trackName, artistName, query.Get("album_name"), duration)
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

		release, ok := fallbacks.enter(r, w)
		if !ok {
			return
		}
		defer release()

		upstreamStart := time.Now()
		remote, remoteErr := client.GetExact(r.Context(), trackName, artistName, query.Get("album_name"), duration)
		setUpstreamDuration(r, time.Since(upstreamStart))
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
			if errors.Is(remoteErr, lrclib.ErrNotFound) && strings.TrimSpace(query.Get("album_name")) != "" {
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
			AlbumName:  firstNonEmpty(remote.AlbumName, query.Get("album_name")),
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
		writeJSON(w, http.StatusOK, toLyricsResponse(track, lyrics))
	}
}

func searchLyricsHandlerWithUpstream(metadataDB, lyricsDB *sql.DB, client *lrclib.Client, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		searchQuery := strings.TrimSpace(query.Get("q"))
		if searchQuery == "" {
			searchQuery = strings.Join(nonEmpty(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name")), " ")
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
		searchQuery := strings.TrimSpace(query.Get("q"))
		if searchQuery == "" {
			searchQuery = strings.Join(nonEmpty(
				query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"),
			), " ")
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
		TrackName:    track.Name,
		ArtistName:   track.ArtistName,
		AlbumName:    track.AlbumName,
		Duration:     track.Duration,
		Instrumental: lyrics.Instrumental,
		PlainLyrics:  lyrics.PlainLyrics,
		SyncedLyrics: lyrics.SyncedLyrics,
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
