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
		if err == nil {
			setOutcome(r, "local_hit")
			writeJSON(w, http.StatusOK, toLyricsResponse(track, lyrics))
			return
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
		remote, err := client.GetExact(r.Context(), trackName, artistName, query.Get("album_name"), duration)
		setUpstreamDuration(r, time.Since(upstreamStart))
		if err != nil {
			if errors.Is(err, lrclib.ErrNotFound) {
				lyricsMisses.Set(missKey, time.Now())
			} else {
				setRequestIssue(r, slog.LevelWarn, err.Error())
			}
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
			return
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
		trackID, _, err := db.InsertTrackWithLyrics(r.Context(), metadataDB, lyricsDB, cachedTrack, db.Lyrics{
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

		track = &cachedTrack
		track.ID = trackID
		lyrics = &db.Lyrics{
			PlainLyrics:  remote.PlainLyrics,
			SyncedLyrics: remote.SyncedLyrics,
			Instrumental: remote.Instrumental,
		}
		setOutcome(r, "lrclib_fallback_hit")
		writeJSON(w, http.StatusOK, toLyricsResponse(track, lyrics))
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
