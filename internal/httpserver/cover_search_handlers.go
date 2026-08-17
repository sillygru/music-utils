package httpserver

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/metadata"
	"github.com/sillygru/music-utils/internal/names"
)

type coverSearchResponse struct {
	EntityType  string `json:"entityType"`
	TrackName   string `json:"trackName,omitempty"`
	ArtistName  string `json:"artistName,omitempty"`
	AlbumName   string `json:"albumName,omitempty"`
	CoverURL    string `json:"coverUrl"`
	CoverSource string `json:"coverUrlSource"`
}

// coverTopResponse is the top-level object served by /api/cover/get for album
// and artist types: the selected cover plus every cached provider result. Song
// responses keep the plain coverSearchResponse shape.
type coverTopResponse struct {
	coverSearchResponse
	Results []coverSearchResponse `json:"results,omitempty"`
}

// searchCoverHandler searches artwork. A free-text q searches songs, albums,
// and artists at once and returns a mixed array where each item carries an
// entityType; q combined with type narrows the search to that kind. Without q
// the structured per-type search (type plus entity-specific fields) is used.
func searchCoverHandler(metadataResolver *metadata.Resolver, resolver *cover.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		q := names.CleanSearch(query.Get("q"))
		kindRaw := strings.TrimSpace(query.Get("type"))

		if q == "" {
			kind, err := coverSearchKind(kindRaw)
			if err != nil {
				setOutcome(r, "bad_request")
				writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "type must be artist, album, or song"})
				return
			}
			cleaned := names.Normalize(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"))
			input := cover.Input{
				TrackName: cleaned.TrackName, ArtistName: cleaned.ArtistName, AlbumName: cleaned.AlbumName,
			}
			if kind == cover.Artist && input.ArtistName == "" {
				setOutcome(r, "bad_request")
				writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "artist_name is required for artist covers"})
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
			limit, ok := coverSearchLimit(w, r, query.Get("limit"))
			if !ok {
				return
			}
			searchCoverStructured(w, r, resolver, fallbacks, fallbackEnabled, kind, input, limit)
			return
		}

		limit, ok := coverSearchLimit(w, r, query.Get("limit"))
		if !ok {
			return
		}
		if kindRaw != "" {
			kind, err := coverSearchKind(kindRaw)
			if err != nil {
				setOutcome(r, "bad_request")
				writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "type must be artist, album, or song"})
				return
			}
			searchCoverFreeText(w, r, metadataResolver, resolver, fallbacks, fallbackEnabled, kind, q, limit)
			return
		}
		searchCoverFreeTextAll(w, r, metadataResolver, resolver, fallbacks, fallbackEnabled, q, limit)
	}
}

// coverSearchLimit parses the optional limit parameter, defaulting to 10.
func coverSearchLimit(w http.ResponseWriter, r *http.Request, raw string) (int, bool) {
	limit := 10
	if raw = strings.TrimSpace(raw); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 50 {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
			return 0, false
		}
		limit = parsed
	}
	return limit, true
}

// searchCoverStructured runs the legacy per-type provider search.
func searchCoverStructured(w http.ResponseWriter, r *http.Request, resolver *cover.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool, kind cover.Kind, input cover.Input, limit int) {
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
	results = filterCoverResults(kind, input, results)
	response := make([]coverSearchResponse, 0, len(results))
	for _, result := range results {
		response = append(response, coverSearchResponse{
			EntityType: kind.String(), TrackName: result.TrackName, ArtistName: result.ArtistName,
			AlbumName: result.AlbumName, CoverURL: result.URL, CoverSource: result.Source,
		})
	}
	writeCoverSearch(w, r, response)
}

// searchCoverFreeText runs a free-text search restricted to one kind: songs via
// the metadata providers, albums and artists via the cover providers.
func searchCoverFreeText(w http.ResponseWriter, r *http.Request, metadataResolver *metadata.Resolver, resolver *cover.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool, kind cover.Kind, q string, limit int) {
	if !fallbackEnabled || resolver == nil || metadataResolver == nil {
		setOutcome(r, "miss")
		writeJSON(w, http.StatusOK, []coverSearchResponse{})
		return
	}
	release, ok := fallbacks.enter(r, w)
	if !ok {
		return
	}
	defer release()
	var results []coverSearchResponse
	switch kind {
	case cover.Song:
		results = coverSearchSongs(r, metadataResolver, q, limit)
	case cover.Album:
		results = coverSearchByKind(r, resolver, cover.Album, cover.Input{AlbumName: q}, limit)
	default:
		results = coverSearchByKind(r, resolver, cover.Artist, cover.Input{ArtistName: q}, limit)
	}
	writeCoverSearch(w, r, results)
}

// searchCoverFreeTextAll searches songs, albums, and artists for the same query
// and merges the results across types, round-robin, up to limit.
func searchCoverFreeTextAll(w http.ResponseWriter, r *http.Request, metadataResolver *metadata.Resolver, resolver *cover.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool, q string, limit int) {
	if !fallbackEnabled || resolver == nil || metadataResolver == nil {
		setOutcome(r, "miss")
		writeJSON(w, http.StatusOK, []coverSearchResponse{})
		return
	}
	release, ok := fallbacks.enter(r, w)
	if !ok {
		return
	}
	defer release()
	songs := coverSearchSongs(r, metadataResolver, q, limit)
	albums := coverSearchByKind(r, resolver, cover.Album, cover.Input{AlbumName: q}, limit)
	artists := coverSearchByKind(r, resolver, cover.Artist, cover.Input{ArtistName: q}, limit)
	writeCoverSearch(w, r, mergeCoverSearch(limit, songs, albums, artists))
}

// coverSearchSongs resolves up to limit song covers from the metadata
// providers' free-text search.
func coverSearchSongs(r *http.Request, resolver *metadata.Resolver, q string, limit int) []coverSearchResponse {
	tracks, err := resolver.Search(r.Context(), q, limit)
	if err != nil {
		return nil
	}
	results := make([]coverSearchResponse, 0, len(tracks))
	for _, track := range tracks {
		if track == nil || track.CoverURL == "" {
			continue
		}
		results = append(results, coverSearchResponse{
			EntityType: cover.Song.String(), TrackName: track.Name, ArtistName: track.ArtistName,
			AlbumName: track.AlbumName, CoverURL: track.CoverURL, CoverSource: track.CoverURLSource,
		})
	}
	return results
}

// coverSearchByKind resolves up to limit album or artist covers from the cover
// providers for a single free-text term.
func coverSearchByKind(r *http.Request, resolver *cover.Resolver, kind cover.Kind, input cover.Input, limit int) []coverSearchResponse {
	results, err := resolver.Search(r.Context(), kind, input, limit)
	if err != nil {
		return nil
	}
	out := make([]coverSearchResponse, 0, len(results))
	for _, result := range results {
		if result.URL == "" {
			continue
		}
		out = append(out, coverSearchResponse{
			EntityType: kind.String(), TrackName: result.TrackName, ArtistName: result.ArtistName,
			AlbumName: result.AlbumName, CoverURL: result.URL, CoverSource: result.Source,
		})
	}
	return out
}

// mergeCoverSearch interleaves results from each group so no single type crowds
// out the others, deduplicating by entity type and names.
func mergeCoverSearch(limit int, groups ...[]coverSearchResponse) []coverSearchResponse {
	merged := make([]coverSearchResponse, 0, limit)
	seen := make(map[string]struct{}, limit)
	add := func(item coverSearchResponse) bool {
		key := item.EntityType + "\x00" + coverNormalize(item.TrackName) + "\x00" + coverNormalize(item.ArtistName) + "\x00" + coverNormalize(item.AlbumName)
		if _, ok := seen[key]; ok {
			return false
		}
		seen[key] = struct{}{}
		return true
	}
	longest := 0
	for _, group := range groups {
		if len(group) > longest {
			longest = len(group)
		}
	}
	for i := 0; i < longest && len(merged) < limit; i++ {
		for _, group := range groups {
			if i < len(group) && len(merged) < limit && add(group[i]) {
				merged = append(merged, group[i])
			}
		}
	}
	return merged
}

func writeCoverSearch(w http.ResponseWriter, r *http.Request, results []coverSearchResponse) {
	if len(results) == 0 {
		setOutcome(r, "miss")
	} else {
		setOutcome(r, "provider_fallback_hit")
	}
	writeJSON(w, http.StatusOK, results)
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
