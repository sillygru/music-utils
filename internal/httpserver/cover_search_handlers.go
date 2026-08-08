package httpserver

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/cover"
)

type coverSearchResponse struct {
	EntityType  string `json:"entityType"`
	TrackName   string `json:"trackName,omitempty"`
	ArtistName  string `json:"artistName,omitempty"`
	AlbumName   string `json:"albumName,omitempty"`
	CoverURL    string `json:"coverUrl"`
	CoverSource string `json:"coverUrlSource"`
}

// searchCoverHandler returns one result per configured artwork provider. The
// response is intentionally an array, matching the search behavior of the
// metadata and lyrics endpoints.
func searchCoverHandler(resolver *cover.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		kind, err := coverSearchKind(query.Get("type"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "type must be artist, album, or song"})
			return
		}
		input := cover.Input{
			TrackName:  strings.TrimSpace(query.Get("track_name")),
			ArtistName: strings.TrimSpace(query.Get("artist_name")),
			AlbumName:  strings.TrimSpace(query.Get("album_name")),
		}
		if input.ArtistName == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "artist_name is required"})
			return
		}
		if kind == cover.Album && input.AlbumName == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "album_name is required for album covers"})
			return
		}
		if kind == cover.Song && input.TrackName == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "track_name is required for song covers"})
			return
		}
		limit := 10
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 50 {
				setOutcome(r, "bad_request")
				writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
				return
			}
		}
		if !fallbackEnabled || resolver == nil {
			setOutcome(r, "miss")
			writeJSON(w, http.StatusOK, []coverSearchResponse{})
			return
		}
		release, ok := fallbacks.enter(r, w)
		if !ok {
			return
		}
		defer release()
		started := time.Now()
		results, lookupErr := resolver.Search(r.Context(), kind, input, limit)
		setUpstreamDuration(r, time.Since(started))
		if lookupErr != nil {
			setOutcome(r, "miss")
			writeJSON(w, http.StatusOK, []coverSearchResponse{})
			return
		}
		response := make([]coverSearchResponse, 0, len(results))
		for _, result := range results {
			response = append(response, coverSearchResponse{
				EntityType: kind.String(), TrackName: result.TrackName, ArtistName: result.ArtistName,
				AlbumName: result.AlbumName, CoverURL: result.URL, CoverSource: result.Source,
			})
		}
		if len(response) == 0 {
			setOutcome(r, "miss")
		} else {
			setOutcome(r, "provider_fallback_hit")
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func coverSearchKind(value string) (cover.Kind, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "artist":
		return cover.Artist, nil
	case "album":
		return cover.Album, nil
	case "song", "track":
		return cover.Song, nil
	default:
		return cover.Artist, strconv.ErrSyntax
	}
}
