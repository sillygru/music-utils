package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/names"
)

func searchLyricsHandlerParallel(metadataDB, lyricsDB *sql.DB, providers *lyricsProviders, fallbacks *fallbackGuard) http.HandlerFunc {
	group := newLyricsSearchGroup()
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		searchQuery := names.CleanSearch(query.Get("q"))
		if searchQuery == "" {
			searchQuery = names.CleanSearch(strings.Join(nonEmpty(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name")), " "))
		}
		if searchQuery == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "q or track_name, artist_name, or album_name is required"})
			return
		}
		limit, err := searchLimit(query.Get("limit"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
			return
		}
		includeRich := includeRichSync(r)
		syncType := requestedRichSyncType(r)
		videoID := sanitizeVideoID(query.Get("video_id"))
		hintTrack := strings.TrimSpace(query.Get("track_name"))
		hintArtist := strings.TrimSpace(query.Get("artist_name"))
		hintAlbum := strings.TrimSpace(query.Get("album_name"))
		cacheKey := lyricsSearchCacheKeyWithVideo(searchQuery, limit, includeRich, syncType, videoID)

		if cached, cacheErr := db.FindLyricsSearchCache(r.Context(), lyricsDB, cacheKey, lyricsSearchCacheTTL); cacheErr == nil {
			var cachedResults []lyricsResponse
			if json.Unmarshal(cached, &cachedResults) == nil {
				setOutcome(r, "local_hit")
				writeJSON(w, http.StatusOK, cachedResults)
				return
			}
		} else if !errors.Is(cacheErr, sql.ErrNoRows) {
			setRequestIssue(r, slog.LevelWarn, cacheErr.Error())
		}

		cacheStart := time.Now()
		localTracks, localErr := db.SearchTracks(r.Context(), metadataDB, lyricsDB, searchQuery, limit)
		setCacheDuration(r, time.Since(cacheStart))
		if localErr != nil {
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		// An exact local catalog hit with rich sync requested should not spend
		// LRCLIB search budget merely to rediscover release variants.
		skipRemote := includeRich && len(localTracks) > 0
		results := group.lookup(r.Context(), cacheKey, func(ctx context.Context, publish func([]lyricsResponse)) {
			runParallelLyricsSearch(ctx, publish, metadataDB, lyricsDB, providers, fallbacks, includeRich, skipRemote, clientIP(r, false), searchQuery, hintTrack, hintArtist, hintAlbum, videoID, limit, cacheKey)
		})
		if results == nil {
			results = []lyricsResponse{}
		}
		sort.SliceStable(results, func(i, j int) bool {
			return searchResponseScore(results[i]) > searchResponseScore(results[j])
		})
		if len(results) > limit {
			results = results[:limit]
		}
		if encoded, marshalErr := json.Marshal(results); marshalErr == nil {
			if cacheErr := db.UpsertLyricsSearchCache(r.Context(), lyricsDB, cacheKey, encoded); cacheErr != nil {
				setRequestIssue(r, slog.LevelWarn, cacheErr.Error())
			}
		}
		if len(results) == 0 {
			setOutcome(r, "miss")
		} else if len(localTracks) > 0 {
			setOutcome(r, "local_hit")
		} else {
			setOutcome(r, "lrclib_fallback_hit")
		}
		writeJSON(w, http.StatusOK, results)
	}
}

func lyricsSearchCacheKeyWithVideo(query string, limit int, includeRich bool, syncType, videoID string) string {
	return lyricsSearchCacheKey(query, limit, includeRich, syncType) + "\x00" + canonicalPart(videoID)
}

// searchProviderTitle resolves the title hint for metadata providers: an
// explicit track_name wins, otherwise the free-text query is used as-is.
func searchProviderTitle(hintTrack, query string) string {
	if hintTrack != "" {
		return hintTrack
	}
	return query
}

// searchPersistResponse stores one provider result and converts it, tagging
// the variant with the provider source for future provenance use.
func searchPersistResponse(ctx context.Context, metadataDB, lyricsDB *sql.DB, result lrclib.RemoteResult, source string) lyricsResponse {
	track := db.Track{Name: result.TrackName, ArtistName: result.ArtistName, AlbumName: result.AlbumName, Duration: result.Duration, Source: source}
	lyrics := db.Lyrics{PlainLyrics: result.PlainLyrics, SyncedLyrics: result.SyncedLyrics, Instrumental: result.Instrumental, Source: source}
	trackID, _, persistErr := db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, lyrics)
	if persistErr == nil {
		track.ID = trackID
	}
	response := toLyricsResponse(&track, &lyrics)
	appendLyricsVariant(&response, &response)
	return response
}
