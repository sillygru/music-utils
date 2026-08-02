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
	ID          int64  `json:"id"`
	EntityType  string `json:"entityType"`
	ArtistName  string `json:"artistName,omitempty"`
	AlbumName   string `json:"albumName,omitempty"`
	CoverURL    string `json:"coverUrl,omitempty"`
	CoverSource string `json:"coverUrlSource,omitempty"`
}

func getArtistCoverHandler(database *sql.DB, resolver *cover.Resolver, fallbackEnabled bool) http.HandlerFunc {
	return handleEntityCover(database, resolver, fallbackEnabled, db.CoverArtist)
}

func getAlbumCoverHandler(database *sql.DB, resolver *cover.Resolver, fallbackEnabled bool) http.HandlerFunc {
	return handleEntityCover(database, resolver, fallbackEnabled, db.CoverAlbum)
}

func handleEntityCover(database *sql.DB, resolver *cover.Resolver, fallbackEnabled bool, entityType db.CoverEntity) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		artist := strings.TrimSpace(r.URL.Query().Get("artist_name"))
		album := strings.TrimSpace(r.URL.Query().Get("album_name"))
		if artist == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "artist_name is required"})
			return
		}
		if entityType == db.CoverAlbum && album == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "album_name is required"})
			return
		}

		cached, err := db.FindCoverArt(r.Context(), database, entityType, artist, album)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		if err == nil {
			if cached.CoverURL != "" {
				setOutcome(r, "local_hit")
				writeJSON(w, http.StatusOK, albumArtistCoverFromRow(cached, entityType, artist, album))
				return
			}
			if checkedRecently(cached.CheckedAt) {
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

		result, lookupErr := resolver.Lookup(r.Context(), toKind(entityType), cover.Input{ArtistName: artist, AlbumName: album})
		if lookupErr != nil {
			// Persist a negative result so repeat lookups stop spending upstream
			// budget for the negative-cache window.
			_ = db.UpsertCoverArt(r.Context(), database, entityType, artist, album, "", "")
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
			return
		}
		if result == nil || result.URL == "" {
			_ = db.UpsertCoverArt(r.Context(), database, entityType, artist, album, "", "")
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
			return
		}
		if err := db.UpsertCoverArt(r.Context(), database, entityType, artist, album, result.URL, result.Source); err != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		setOutcome(r, "provider_fallback_hit")
		writeJSON(w, http.StatusOK, albumArtistCoverResponse{
			EntityType: string(entityType), ArtistName: artist, AlbumName: album,
			CoverURL: result.URL, CoverSource: result.Source,
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

func albumArtistCoverFromRow(row *db.CoverArt, entityType db.CoverEntity, artist, album string) albumArtistCoverResponse {
	return albumArtistCoverResponse{
		ID: row.ID, EntityType: string(entityType), ArtistName: artist, AlbumName: album,
		CoverURL: row.CoverURL, CoverSource: row.CoverSource,
	}
}
