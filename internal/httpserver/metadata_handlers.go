package httpserver

import (
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
)

type metadataResponse struct {
	ID                        int64   `json:"id"`
	TrackName                 string  `json:"trackName"`
	ArtistName                string  `json:"artistName"`
	AlbumName                 string  `json:"albumName"`
	Duration                  float64 `json:"duration"`
	Genre                     string  `json:"genre,omitempty"`
	Year                      int     `json:"year,omitempty"`
	ReleaseDate               string  `json:"releaseDate,omitempty"`
	ISRC                      string  `json:"isrc,omitempty"`
	MusicBrainzRecordingID    string  `json:"musicbrainzRecordingId,omitempty"`
	MusicBrainzReleaseID      string  `json:"musicbrainzReleaseId,omitempty"`
	MusicBrainzReleaseGroupID string  `json:"musicbrainzReleaseGroupId,omitempty"`
	MusicBrainzArtistID       string  `json:"musicbrainzArtistId,omitempty"`
	CoverURL                  string  `json:"coverUrl,omitempty"`
	MetadataSource            string  `json:"metadataSource,omitempty"`
	CoverURLSource            string  `json:"coverUrlSource,omitempty"`
}

func getMetadataHandler(database *sql.DB, resolver *metadata.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		name := strings.TrimSpace(query.Get("track_name"))
		artist := strings.TrimSpace(query.Get("artist_name"))
		if name == "" {
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
		album := query.Get("album_name")
		cacheStart := time.Now()
		local, err := db.FindTrackMetadataExact(r.Context(), database, name, artist, album, duration)
		setCacheDuration(r, time.Since(cacheStart))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		if err == nil && local.MetadataChecked {
			setOutcome(r, "local_hit")
			writeJSON(w, http.StatusOK, toMetadataResponse(local))
			return
		}
		if !fallbackEnabled || resolver == nil {
			if local != nil {
				setOutcome(r, "local_partial_hit")
				writeJSON(w, http.StatusOK, toMetadataResponse(local))
				return
			}
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
		remote, err := resolver.Lookup(r.Context(), metadata.Input{TrackName: name, ArtistName: artist, AlbumName: album, Duration: duration})
		setUpstreamDuration(r, time.Since(upstreamStart))
		if err != nil {
			if !errors.Is(err, metadata.ErrNotFound) {
				setRequestIssue(r, slog.LevelWarn, err.Error())
			}
			if local != nil {
				setOutcome(r, "local_partial_hit")
				writeJSON(w, http.StatusOK, toMetadataResponse(local))
				return
			}
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
			return
		}
		if local != nil && local.Duration > 0 {
			remote.Duration = local.Duration
		}
		if remote.AlbumName == "" {
			remote.AlbumName = album
		}
		remote.MetadataChecked = true
		trackID, err := db.UpsertTrackMetadata(r.Context(), database, *remote)
		if err != nil {
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		remote.ID = trackID
		setOutcome(r, "provider_fallback_hit")
		writeJSON(w, http.StatusOK, toMetadataResponse(remote))
	}
}

func searchMetadataHandlerWithUpstream(database *sql.DB, resolver *metadata.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		searchQuery := strings.TrimSpace(query.Get("q"))
		if searchQuery == "" {
			searchQuery = strings.Join(nonEmpty(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"), query.Get("genre")), " ")
		}
		if searchQuery == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "q or track_name, artist_name, album_name, or genre is required"})
			return
		}
		limit, err := searchLimit(query.Get("limit"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
			return
		}
		cacheStart := time.Now()
		tracks, err := db.SearchTracks(r.Context(), database, nil, searchQuery, limit)
		setCacheDuration(r, time.Since(cacheStart))
		if err != nil {
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		results := make([]metadataResponse, 0, limit)
		seen := make(map[string]struct{}, limit)
		appendTrack := func(track *db.Track) {
			if track == nil || len(results) >= limit {
				return
			}
			key := strings.ToLower(strings.TrimSpace(track.Name)) + "\x00" + strings.ToLower(strings.TrimSpace(track.ArtistName)) + "\x00" + strings.ToLower(strings.TrimSpace(track.AlbumName))
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
			results = append(results, toMetadataResponse(track))
		}
		// Put provider results first so a warm local catalog cannot hide the
		// requested upstream APIs merely because it fills the final limit.
		if fallbackEnabled && resolver != nil {
			release, ok := fallbacks.enter(r, w)
			if !ok {
				return
			}
			defer release()
			upstreamStart := time.Now()
			remote, remoteErr := resolver.Search(r.Context(), searchQuery, limit)
			setUpstreamDuration(r, time.Since(upstreamStart))
			if remoteErr == nil {
				for _, track := range remote {
					appendTrack(track)
				}
			}
		}
		for i := range tracks {
			appendTrack(&tracks[i].Track)
		}
		if len(results) == 0 {
			setOutcome(r, "miss")
		} else if len(tracks) > 0 {
			setOutcome(r, "local_hit")
		} else {
			setOutcome(r, "provider_fallback_hit")
		}
		writeJSON(w, http.StatusOK, results)
	}
}

func searchMetadataHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		searchQuery := strings.TrimSpace(query.Get("q"))
		if searchQuery == "" {
			searchQuery = strings.Join(nonEmpty(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"), query.Get("genre")), " ")
		}
		if searchQuery == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "q or track_name, artist_name, album_name, or genre is required"})
			return
		}
		limit, err := searchLimit(query.Get("limit"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
			return
		}
		cacheStart := time.Now()
		tracks, err := db.SearchTracks(r.Context(), database, nil, searchQuery, limit)
		setCacheDuration(r, time.Since(cacheStart))
		if err != nil {
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		result := make([]metadataResponse, 0, len(tracks))
		for i := range tracks {
			result = append(result, toMetadataResponse(&tracks[i].Track))
		}
		if len(result) == 0 {
			setOutcome(r, "miss")
		} else {
			setOutcome(r, "local_hit")
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func toMetadataResponse(track *db.Track) metadataResponse {
	return metadataResponse{
		ID: track.ID, TrackName: track.Name, ArtistName: track.ArtistName, AlbumName: track.AlbumName,
		Duration: track.Duration, Genre: track.Genre, Year: track.Year, ReleaseDate: track.ReleaseDate,
		ISRC: track.ISRC, MusicBrainzRecordingID: track.MusicBrainzRecordingID,
		MusicBrainzReleaseID: track.MusicBrainzReleaseID, MusicBrainzReleaseGroupID: track.MusicBrainzReleaseGroupID,
		MusicBrainzArtistID: track.MusicBrainzArtistID, CoverURL: track.CoverURL,
		MetadataSource: track.MetadataSource, CoverURLSource: track.CoverURLSource,
	}
}

func optionalDuration(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	duration, err := strconv.ParseFloat(value, 64)
	if err != nil || duration < 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return 0, errors.New("invalid duration")
	}
	return duration, nil
}
