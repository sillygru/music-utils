package httpserver

import (
	"database/sql"
	"net/http"

	"github.com/sillygru/music-utils/internal/db"
)

// API paths under /api/stats/ reporting how many songs are cached, split by
// cache. Each endpoint is registered only when its name is selected through the
// STATS_ENDPOINTS environment variable, and requests to /api/stats/* are never
// written to the request log database (see requestLogger).
const (
	statsMetadataPath = "/api/stats/metadata"
	statsLyricsPath   = "/api/stats/lyrics"
	statsCoversPath   = "/api/stats/covers"
	statsTotalPath    = "/api/stats/total"
	statsSongsPath    = "/api/stats/songs"
)

// statsFailure reports a failed cache-count query as a generic 500 so the
// endpoint never leaks query internals.
func statsFailure(w http.ResponseWriter) {
	writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
}

// statsMetadataHandler reports how many songs have cached metadata.
func statsMetadataHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOutcome(r, "local_hit")
		count, err := db.CountTracks(r.Context(), database)
		if err != nil {
			setOutcome(r, "error")
			statsFailure(w)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			MetadataSongs int64 `json:"metadataSongs"`
		}{MetadataSongs: count})
	}
}

// statsLyricsHandler reports how many songs have cached lyrics.
func statsLyricsHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOutcome(r, "local_hit")
		count, err := db.CountLyricsTracks(r.Context(), database)
		if err != nil {
			setOutcome(r, "error")
			statsFailure(w)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			LyricsSongs int64 `json:"lyricsSongs"`
		}{LyricsSongs: count})
	}
}

// statsSongsHandler reports how many distinct individual track names are cached.
func statsSongsHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOutcome(r, "local_hit")
		count, err := db.CountDistinctTrackNames(r.Context(), database)
		if err != nil {
			setOutcome(r, "error")
			statsFailure(w)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Songs int64 `json:"songs"`
		}{Songs: count})
	}
}

// statsCoversHandler reports the total amount of cached cover entries, broken
// down into song covers (from the metadata cache), album covers, and artist
// covers (from the cover URL cache). Negative cache rows with an empty URL are
// not counted.
func statsCoversHandler(metadataDB, coverDB *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOutcome(r, "local_hit")
		counts, err := db.CountCovers(r.Context(), metadataDB, coverDB)
		if err != nil {
			setOutcome(r, "error")
			statsFailure(w)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Covers       int64 `json:"covers"`
			SongCovers   int64 `json:"songCovers"`
			AlbumCovers  int64 `json:"albumCovers"`
			ArtistCovers int64 `json:"artistCovers"`
		}{Covers: counts.Total(), SongCovers: counts.Songs, AlbumCovers: counts.Albums, ArtistCovers: counts.Artists})
	}
}

// statsTotalHandler reports the unified total of everything cached: metadata,
// lyrics, and covers summed together, with the per-cache breakdown alongside.
func statsTotalHandler(metadataDB, lyricsDB, coverDB *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOutcome(r, "local_hit")
		metadata, err := db.CountTracks(r.Context(), metadataDB)
		if err != nil {
			setOutcome(r, "error")
			statsFailure(w)
			return
		}
		lyrics, err := db.CountLyricsTracks(r.Context(), lyricsDB)
		if err != nil {
			setOutcome(r, "error")
			statsFailure(w)
			return
		}
		counts, err := db.CountCovers(r.Context(), metadataDB, coverDB)
		if err != nil {
			setOutcome(r, "error")
			statsFailure(w)
			return
		}
		covers := counts.Total()
		writeJSON(w, http.StatusOK, struct {
			Total    int64 `json:"total"`
			Metadata int64 `json:"metadata"`
			Lyrics   int64 `json:"lyrics"`
			Covers   int64 `json:"covers"`
		}{Total: metadata + lyrics + covers, Metadata: metadata, Lyrics: lyrics, Covers: covers})
	}
}
