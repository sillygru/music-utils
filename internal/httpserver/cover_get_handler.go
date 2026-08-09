package httpserver

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
)

func getCoverTopHandler(metadataDB, coverDB *sql.DB, resolver *cover.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		kind := cover.Song
		var err error
		if strings.TrimSpace(query.Get("type")) != "" {
			kind, err = coverSearchKind(query.Get("type"))
		}
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "type must be artist, album, or song"})
			return
		}
		input := cover.Input{
			TrackName: strings.TrimSpace(query.Get("track_name")), ArtistName: strings.TrimSpace(query.Get("artist_name")), AlbumName: strings.TrimSpace(query.Get("album_name")),
		}
		switch kind {
		case cover.Artist:
			if input.ArtistName == "" {
				setOutcome(r, "bad_request")
				writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "artist_name is required for artist covers"})
				return
			}
		case cover.Album:
			if input.AlbumName == "" {
				setOutcome(r, "bad_request")
				writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "album_name is required for album covers"})
				return
			}
		default:
			if input.TrackName == "" {
				setOutcome(r, "bad_request")
				writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "track_name is required for song covers"})
				return
			}
		}

		// A song get first uses the existing metadata cache, which is cheaper
		// and preserves the exact cover URL chosen during metadata enrichment.
		if (kind == cover.Artist || kind == cover.Album) && coverDB != nil {
			entity := coverEntityForKind(kind)
			cacheStart := time.Now()
			cached, lookupErr := db.FindCoverArt(r.Context(), coverDB, entity, input.ArtistName, input.AlbumName)
			setCacheDuration(r, time.Since(cacheStart))
			if lookupErr == nil && cached.CoverURL != "" {
				setOutcome(r, "local_hit")
				writeJSON(w, http.StatusOK, coverSearchResponse{EntityType: kind.String(), ArtistName: input.ArtistName, AlbumName: input.AlbumName, CoverURL: cached.CoverURL, CoverSource: cached.CoverSource})
				return
			}
			if lookupErr == nil && cached.CoverURL == "" && checkedRecently(cached.CheckedAt) {
				// Fresh negative cache: do not spend upstream budget again.
				setOutcome(r, "miss")
				writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
				return
			}
			if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
				setOutcome(r, "error")
				writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
				return
			}
		}

		if kind == cover.Song && metadataDB != nil {
			cacheStart := time.Now()
			track, lookupErr := db.FindTrackMetadataExact(r.Context(), metadataDB, input.TrackName, input.ArtistName, input.AlbumName, 0)
			setCacheDuration(r, time.Since(cacheStart))
			if lookupErr == nil && track.CoverURL != "" {
				setOutcome(r, "local_hit")
				writeJSON(w, http.StatusOK, coverSearchResponse{EntityType: kind.String(), TrackName: track.Name, ArtistName: track.ArtistName, AlbumName: track.AlbumName, CoverURL: track.CoverURL, CoverSource: track.CoverURLSource})
				return
			}
			if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
				setOutcome(r, "error")
				writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
				return
			}
		}
		if !fallbackEnabled || resolver == nil {
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
			return
		}
		release, ok := fallbacks.enter(r, w)
		if !ok {
			return
		}
		defer release()
		started := time.Now()
		var result *cover.Result
		var lookupErr error
		if kind == cover.Song {
			result, lookupErr = resolver.Lookup(r.Context(), kind, input)
		} else {
			// Album and artist names can be ambiguous, so gather every provider's
			// candidates and keep the first that plausibly matches the request
			// instead of trusting each provider's single top result.
			results, err := resolver.Search(r.Context(), kind, input, 50)
			lookupErr = err
			if err == nil {
				results = filterCoverResults(kind, input, results)
			}
			if len(results) > 0 {
				top := results[0]
				result = &top
			}
		}
		setUpstreamDuration(r, time.Since(started))
		if lookupErr != nil || result == nil || result.URL == "" {
			if kind != cover.Song && coverDB != nil {
				_ = db.UpsertCoverArt(r.Context(), coverDB, coverEntityForKind(kind), input.ArtistName, input.AlbumName, "", "")
			}
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
			return
		}
		if kind != cover.Song && coverDB != nil {
			_ = db.UpsertCoverArt(r.Context(), coverDB, coverEntityForKind(kind), input.ArtistName, input.AlbumName, result.URL, result.Source)
		}
		setOutcome(r, "provider_fallback_hit")
		writeJSON(w, http.StatusOK, coverSearchResponse{EntityType: kind.String(), TrackName: result.TrackName, ArtistName: result.ArtistName, AlbumName: result.AlbumName, CoverURL: result.URL, CoverSource: result.Source})
	}
}
