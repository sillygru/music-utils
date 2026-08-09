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
// configured provider result. A cached positive row is served immediately so
// repeat lookups never spend upstream budget; providers are only consulted on
// a genuine miss (no cached URL, or a negative result older than the
// negative-cache window).
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
		if cacheErr == nil && cached.CoverURL != "" {
			// Positive cache hit: serve the saved cover immediately and never
			// consult upstream. Staleness and dead URLs are handled in the
			// background by the cover refresh job. Variants are best-effort:
			// a variant read failure still serves the cached winner.
			setOutcome(r, "local_hit")
			variants, _ := db.FindCoverVariants(r.Context(), database, cached.ID)
			writeJSON(w, http.StatusOK, albumArtistCoverFromRow(cached, entityType, artist, album, variants))
			return
		}
		if cacheErr == nil && cached.CoverURL == "" && checkedRecently(cached.CheckedAt) {
			// Fresh negative cache: do not spend upstream budget again.
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
			return
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
		results, lookupErr := resolver.Search(r.Context(), toKind(entityType), cover.Input{ArtistName: artist, AlbumName: album}, 50)
		setUpstreamDuration(r, time.Since(started))
		if lookupErr == nil {
			results = filterCoverResults(toKind(entityType), cover.Input{ArtistName: artist, AlbumName: album}, results)
		}
		if lookupErr != nil || len(results) == 0 {
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
		// Persist every plausible provider URL, not just the winner, so the
		// cache can serve alternates (or promote a live one) later.
		variants := make([]db.CoverVariant, 0, len(results))
		for _, result := range results {
			variants = append(variants, db.CoverVariant{URL: result.URL, Source: result.Source})
		}
		if err := db.UpsertCoverArtVariants(r.Context(), database, entityType, artist, album, variants); err != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		setOutcome(r, "provider_fallback_hit")
		writeJSON(w, http.StatusOK, albumArtistCoverResponse{
			EntityType: string(entityType), ArtistName: artist, AlbumName: album,
			CoverURL: variants[0].URL, CoverSource: variants[0].Source, Results: providerResults,
		})
	}
}
