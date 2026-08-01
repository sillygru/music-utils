package httpserver

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/sillygru/music-utils/internal/db"
)

type coverResponse struct {
	ID             int64  `json:"id"`
	TrackName      string `json:"trackName"`
	ArtistName     string `json:"artistName"`
	AlbumName      string `json:"albumName"`
	CoverURL       string `json:"coverUrl"`
	CoverURLSource string `json:"coverUrlSource"`
}

// getCoverHandler serves only cached artwork. Metadata lookup is intentionally
// separate so clients can choose whether a cover miss should spend upstream
// MusicBrainz/Cover Art Archive budget.
func getCoverHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		name, artist := strings.TrimSpace(query.Get("track_name")), strings.TrimSpace(query.Get("artist_name"))
		if name == "" || artist == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "track_name and artist_name are required"})
			return
		}
		track, err := db.FindTrackMetadataExact(r.Context(), database, name, artist, query.Get("album_name"), 0)
		if errors.Is(err, sql.ErrNoRows) || err == nil && track.CoverURL == "" {
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Cover not found"})
			return
		}
		if err != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		setOutcome(r, "local_hit")
		writeJSON(w, http.StatusOK, coverResponse{ID: track.ID, TrackName: track.Name, ArtistName: track.ArtistName, AlbumName: track.AlbumName, CoverURL: track.CoverURL, CoverURLSource: track.CoverURLSource})
	}
}
