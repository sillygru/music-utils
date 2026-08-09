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

const lastFMTimeFormat = "2006-01-02 15:04:05"

type albumArtistCoverResponse struct {
	ID          int64                 `json:"id"`
	EntityType  string                `json:"entityType"`
	ArtistName  string                `json:"artistName,omitempty"`
	AlbumName   string                `json:"albumName,omitempty"`
	CoverURL    string                `json:"coverUrl,omitempty"`
	CoverSource string                `json:"coverUrlSource,omitempty"`
	Results     []coverSearchResponse `json:"results,omitempty"`
}

func getArtistCoverHandler(database *sql.DB, resolver *cover.Resolver, fallbacks *fallbackGuard, refreshAfter time.Duration, fallbackEnabled bool) http.HandlerFunc {
	return handleEntityCover(database, resolver, fallbacks, refreshAfter, fallbackEnabled, db.CoverArtist)
}

func getAlbumCoverHandler(database *sql.DB, resolver *cover.Resolver, fallbacks *fallbackGuard, refreshAfter time.Duration, fallbackEnabled bool) http.HandlerFunc {
	return handleEntityCover(database, resolver, fallbacks, refreshAfter, fallbackEnabled, db.CoverAlbum)
}

func handleEntityCover(database *sql.DB, resolver *cover.Resolver, fallbacks *fallbackGuard, refreshAfter time.Duration, fallbackEnabled bool, entityType db.CoverEntity) http.HandlerFunc {
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
		cached, err := db.FindCoverArt(r.Context(), database, entityType, artist, album)
		setCacheDuration(r, time.Since(cacheStart))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		if err == nil {
			if cached.CoverURL != "" && !coverPositiveStale(cached.CheckedAt, refreshAfter) {
				setOutcome(r, "local_hit")
				writeJSON(w, http.StatusOK, albumArtistCoverFromRow(cached, entityType, artist, album))
				return
			}
			if cached.CoverURL != "" && (!fallbackEnabled || resolver == nil) {
				// Stale positive with no fallback to refresh it: serving the
				// cached URL beats returning 404.
				setOutcome(r, "local_hit")
				writeJSON(w, http.StatusOK, albumArtistCoverFromRow(cached, entityType, artist, album))
				return
			}
			if cached.CoverURL == "" && checkedRecently(cached.CheckedAt) {
				// Fresh negative cache: do not spend upstream budget again.
				setOutcome(r, "miss")
				writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
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

		upstreamStart := time.Now()
		results, lookupErr := resolver.Search(r.Context(), toKind(entityType), cover.Input{ArtistName: artist, AlbumName: album}, 50)
		setUpstreamDuration(r, time.Since(upstreamStart))
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
		result := results[0]
		if err := db.UpsertCoverArt(r.Context(), database, entityType, artist, album, result.URL, result.Source); err != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		setOutcome(r, "provider_fallback_hit")
		providerResults := make([]coverSearchResponse, 0, len(results))
		for _, item := range results {
			providerResults = append(providerResults, coverSearchResponse{
				EntityType: toKind(entityType).String(), TrackName: item.TrackName,
				ArtistName: item.ArtistName, AlbumName: item.AlbumName,
				CoverURL: item.URL, CoverSource: item.Source,
			})
		}
		writeJSON(w, http.StatusOK, albumArtistCoverResponse{
			EntityType: string(entityType), ArtistName: artist, AlbumName: album,
			CoverURL: result.URL, CoverSource: result.Source, Results: providerResults,
		})
	}
}

func toKind(entityType db.CoverEntity) cover.Kind {
	if entityType == db.CoverAlbum {
		return cover.Album
	}
	return cover.Artist
}

func checkedRecently(checkedAt string) bool {
	if checkedAt == "" {
		return false
	}
	checked, err := time.Parse(lastFMTimeFormat, checkedAt)
	if err != nil {
		return false
	}
	return time.Since(checked) < cover.NegativeCacheTTL
}

// coverPositiveStale reports whether a cached positive cover row should be
// re-resolved because its last check is older than refreshAfter. A zero or
// negative refreshAfter disables staleness (always serve the cached URL).
func coverPositiveStale(checkedAt string, refreshAfter time.Duration) bool {
	if refreshAfter <= 0 {
		return false
	}
	if checkedAt == "" {
		return true
	}
	checked, err := time.Parse(lastFMTimeFormat, checkedAt)
	if err != nil {
		return false
	}
	return time.Since(checked) > refreshAfter
}

func albumArtistCoverFromRow(row *db.CoverArt, entityType db.CoverEntity, artist, album string) albumArtistCoverResponse {
	return albumArtistCoverResponse{
		ID: row.ID, EntityType: string(entityType), ArtistName: artist, AlbumName: album,
		CoverURL: row.CoverURL, CoverSource: row.CoverSource,
	}
}
