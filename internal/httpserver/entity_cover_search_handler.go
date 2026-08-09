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

// getEntityCoverSearchHandler is the public artist/album route contract: the
// legacy top-level fields remain available, while Results exposes every
// configured provider result. A cached row is used when fallback is disabled;
// otherwise providers are queried so a warm cache cannot hide them.
func getEntityCoverSearchHandler(database *sql.DB, resolver *cover.Resolver, fallbacks *fallbackGuard, entityType db.CoverEntity, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		artist := strings.TrimSpace(r.URL.Query().Get("artist_name"))
		album := strings.TrimSpace(r.URL.Query().Get("album_name"))
		if entityType == db.CoverArtist && artist == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "artist_name is required"})
			return
		}
		if entityType == db.CoverAlbum && album == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "album_name is required"})
			return
		}
		cacheStart := time.Now()
		cached, cacheErr := db.FindCoverArt(r.Context(), database, entityType, artist, album)
		setCacheDuration(r, time.Since(cacheStart))
		if cacheErr != nil && !errors.Is(cacheErr, sql.ErrNoRows) {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		if cacheErr == nil && cached.CoverURL == "" && checkedRecently(cached.CheckedAt) {
			// Fresh negative cache: do not spend upstream budget again.
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
			return
		}
		if !fallbackEnabled || resolver == nil {
			if cacheErr == nil && cached.CoverURL != "" {
				setOutcome(r, "local_hit")
				writeJSON(w, http.StatusOK, albumArtistCoverFromRow(cached, entityType, artist, album))
				return
			}
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
		results, lookupErr := resolver.Search(r.Context(), toKind(entityType), cover.Input{ArtistName: artist, AlbumName: album}, 50)
		setUpstreamDuration(r, time.Since(started))
		if lookupErr == nil {
			results = filterCoverResults(toKind(entityType), cover.Input{ArtistName: artist, AlbumName: album}, results)
		}
		if lookupErr != nil || len(results) == 0 {
			if cacheErr == nil && cached.CoverURL != "" {
				setOutcome(r, "local_partial_hit")
				writeJSON(w, http.StatusOK, albumArtistCoverFromRow(cached, entityType, artist, album))
				return
			}
			// Persist a negative result so repeat lookups stop spending upstream
			// budget for the negative-cache window.
			_ = db.UpsertCoverArt(r.Context(), database, entityType, artist, album, "", "")
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
			return
		}
		providerResults := make([]coverSearchResponse, 0, len(results))
		for _, result := range results {
			providerResults = append(providerResults, coverSearchResponse{
				EntityType: toKind(entityType).String(), TrackName: result.TrackName,
				ArtistName: result.ArtistName, AlbumName: result.AlbumName,
				CoverURL: result.URL, CoverSource: result.Source,
			})
		}
		top := results[0]
		if err := db.UpsertCoverArt(r.Context(), database, entityType, artist, album, top.URL, top.Source); err != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		setOutcome(r, "provider_fallback_hit")
		writeJSON(w, http.StatusOK, albumArtistCoverResponse{
			EntityType: string(entityType), ArtistName: artist, AlbumName: album,
			CoverURL: top.URL, CoverSource: top.Source, Results: providerResults,
		})
	}
}
