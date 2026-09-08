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
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/names"
	"github.com/sillygru/music-utils/internal/ttml"
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
				if includeRich {
					for i := range cachedResults {
						if cachedResults[i].ID > 0 {
							if rich, err := db.FindRichLyrics(r.Context(), lyricsDB, cachedResults[i].ID, syncType); err == nil {
								if !isWordRichEmpty(rich) {
									setRichOnlyResponse(&cachedResults[i], rich)
								}
							} else if enriched := tryEnrichSearchResultFromDB(r, metadataDB, lyricsDB, &cachedResults[i], syncType); enriched {
							} else if byName, err := db.FindRichLyricsByName(r.Context(), metadataDB, lyricsDB, cachedResults[i].TrackName, cachedResults[i].ArtistName, syncType); err == nil && !isWordRichEmpty(byName) {
								setRichOnlyResponse(&cachedResults[i], byName)
							} else if syncType != "" {
								if byName2, err := db.FindRichLyricsByName(r.Context(), metadataDB, lyricsDB, cachedResults[i].TrackName, cachedResults[i].ArtistName, ""); err == nil && !isWordRichEmpty(byName2) {
									setRichOnlyResponse(&cachedResults[i], byName2)
								}
							}
						}
					}
				}
				for i := range cachedResults {
					if isWordRichEmptyRichSyncResult(cachedResults[i].RichSync) {
						cachedResults[i].RichSync = nil
					}
					cachedResults[i].SyncedLyrics = ttml.CleanSyncedLyrics(cachedResults[i].SyncedLyrics)
					if cachedResults[i].PlainLyrics == "" && cachedResults[i].SyncedLyrics != "" {
						cachedResults[i].PlainLyrics = ttml.ExtractPlainFromLRC(cachedResults[i].SyncedLyrics)
					}
					if cachedResults[i].PlainLyrics == "" || cachedResults[i].SyncedLyrics == "" {
						for _, v := range cachedResults[i].Variants {
							if cachedResults[i].PlainLyrics == "" && v.PlainLyrics != "" {
								cachedResults[i].PlainLyrics = v.PlainLyrics
							}
							if cachedResults[i].SyncedLyrics == "" && v.SyncedLyrics != "" {
								cachedResults[i].SyncedLyrics = ttml.CleanSyncedLyrics(v.SyncedLyrics)
							}
						}
						if cachedResults[i].PlainLyrics == "" && cachedResults[i].SyncedLyrics != "" {
							cachedResults[i].PlainLyrics = ttml.ExtractPlainFromLRC(cachedResults[i].SyncedLyrics)
						}
					}
				}
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
		skipRemote := includeRich && len(localTracks) > 0
		results := group.lookup(r.Context(), cacheKey, func(ctx context.Context, publish func([]lyricsResponse)) {
			runParallelLyricsSearch(ctx, publish, metadataDB, lyricsDB, providers, fallbacks, includeRich, syncType, skipRemote, clientIP(r, false), searchQuery, hintTrack, hintArtist, hintAlbum, videoID, limit, cacheKey)
		})
		if results == nil {
			results = []lyricsResponse{}
		}
		for i := range results {
			if isWordRichEmptyRichSyncResult(results[i].RichSync) {
				results[i].RichSync = nil
			}
			results[i].SyncedLyrics = ttml.CleanSyncedLyrics(results[i].SyncedLyrics)
			if results[i].PlainLyrics == "" && results[i].SyncedLyrics != "" {
				results[i].PlainLyrics = ttml.ExtractPlainFromLRC(results[i].SyncedLyrics)
			}
			if results[i].PlainLyrics == "" || results[i].SyncedLyrics == "" {
				for _, v := range results[i].Variants {
					if results[i].PlainLyrics == "" && v.PlainLyrics != "" {
						results[i].PlainLyrics = v.PlainLyrics
					}
					if results[i].SyncedLyrics == "" && v.SyncedLyrics != "" {
						results[i].SyncedLyrics = ttml.CleanSyncedLyrics(v.SyncedLyrics)
					}
				}
				if results[i].PlainLyrics == "" && results[i].SyncedLyrics != "" {
					results[i].PlainLyrics = ttml.ExtractPlainFromLRC(results[i].SyncedLyrics)
				}
			}
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
	cleanSynced := ttml.CleanSyncedLyrics(result.SyncedLyrics)
	plain := result.PlainLyrics
	if plain == "" && cleanSynced != "" {
		plain = ttml.ExtractPlainFromLRC(cleanSynced)
	}
	lyrics := db.Lyrics{PlainLyrics: plain, SyncedLyrics: cleanSynced, Instrumental: result.Instrumental, Source: source}
	trackID, _, persistErr := db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, lyrics)
	if persistErr == nil {
		track.ID = trackID
	}
	response := toLyricsResponse(&track, &lyrics)
	appendLyricsVariant(&response, &response)
	return response
}

func tryEnrichSearchResultFromDB(r *http.Request, metadataDB, lyricsDB *sql.DB, response *lyricsResponse, syncType string) bool {
	if response == nil || response.ID <= 0 || lyricsDB == nil {
		return false
	}
	if rich, err := db.FindRichLyrics(r.Context(), lyricsDB, response.ID, syncType); err == nil {
		if isWordRichEmpty(rich) {
			return false
		}
		setRichOnlyResponse(response, rich)
		return true
	}
	if syncType != "" {
		if rich, err := db.FindRichLyrics(r.Context(), lyricsDB, response.ID, ""); err == nil {
			if isWordRichEmpty(rich) {
				return false
			}
			setRichOnlyResponse(response, rich)
			return true
		}
	}
	if metadataDB != nil && response.ArtistName != "" {
		if byName, err := db.FindRichLyricsByName(r.Context(), metadataDB, lyricsDB, response.TrackName, response.ArtistName, syncType); err == nil && !isWordRichEmpty(byName) {
			setRichOnlyResponse(response, byName)
			return true
		}
		if syncType != "" {
			if byName, err := db.FindRichLyricsByName(r.Context(), metadataDB, lyricsDB, response.TrackName, response.ArtistName, ""); err == nil && !isWordRichEmpty(byName) {
				setRichOnlyResponse(response, byName)
				return true
			}
		}
	}
	return false
}

func fetchLiveRichForSearchResults(ctx context.Context, lyricsDB *sql.DB, providers *lyricsProviders, fallbacks *fallbackGuard, results []lyricsResponse, syncType, clientKey string) error {
	if providers == nil || lyricsDB == nil || len(results) == 0 {
		return nil
	}

	type candidate struct {
		rich     *db.RichLyrics
		priority int
	}

	var resultsWG sync.WaitGroup
	for i := range results {
		if results[i].RichSync != nil || results[i].ID <= 0 {
			continue
		}
		resultsWG.Add(1)
		go func(idx int) {
			defer resultsWG.Done()
			trackID := results[idx].ID
			trackName := results[idx].TrackName
			artistName := results[idx].ArtistName
			albumName := results[idx].AlbumName
			duration := results[idx].Duration

			var best candidate
			var mu sync.Mutex
			var wg sync.WaitGroup

			updateBest := func(c candidate) {
				mu.Lock()
				defer mu.Unlock()
				if c.rich == nil || isWordRichEmpty(c.rich) {
					return
				}
				if best.rich == nil || c.priority > best.priority {
					best = c
				}
			}

			// LyricsPlus (priority 3)
			if providers.lyricsPlusEnabled && providers.lyricsPlus != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if fallbacks != nil {
						release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
						if !ok {
							return
						}
						defer release()
					}
					remote, err := providers.lyricsPlus.Get(ctx, trackName, artistName, albumName, duration, "")
					if err != nil || remote == nil || !remote.WordSynced {
						return
					}
					var rich db.RichLyrics
					if strings.TrimSpace(remote.TTML) != "" {
						content, format, converted := compactRichSyncForStorage(remote.TTML, "ttml")
						if !converted {
							content, format = remote.TTML, "ttml"
						}
						rich = db.RichLyrics{TrackID: trackID, Content: content, Format: format, SyncType: "word", Source: "lyricsplus"}
					} else if strings.TrimSpace(remote.RichJSON) != "" {
						rich = db.RichLyrics{TrackID: trackID, Content: remote.RichJSON, Format: "json", SyncType: "word", Source: "lyricsplus"}
					}
					if rich.Content != "" && !isWordRichEmpty(&rich) {
						if err := db.UpsertRichLyrics(ctx, lyricsDB, rich); err == nil {
							if stored, err := db.FindRichLyrics(ctx, lyricsDB, trackID, "word"); err == nil && !isWordRichEmpty(stored) {
								updateBest(candidate{rich: stored, priority: 3})
							} else {
								updateBest(candidate{rich: &rich, priority: 3})
							}
						}
					}
				}()
			}

			// Paxsenix (priority 2)
			if providers.paxsenixEnabled && providers.paxsenix != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if fallbacks != nil {
						release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
						if !ok {
							return
						}
						defer release()
					}
					remote, err := providers.paxsenix.Get(ctx, trackName, artistName, albumName)
					if err != nil || remote == nil || !remote.WordSynced || strings.TrimSpace(remote.TTML) == "" {
						return
					}
					content, format, converted := compactRichSyncForStorage(remote.TTML, "ttml")
					if !converted {
						content, format = remote.TTML, "ttml"
					}
					rich := db.RichLyrics{TrackID: trackID, Content: content, Format: format, SyncType: "word", Source: "paxsenix"}
					if !isWordRichEmpty(&rich) {
						if err := db.UpsertRichLyrics(ctx, lyricsDB, rich); err == nil {
							if stored, err := db.FindRichLyrics(ctx, lyricsDB, trackID, "word"); err == nil && !isWordRichEmpty(stored) {
								updateBest(candidate{rich: stored, priority: 2})
							} else {
								updateBest(candidate{rich: &rich, priority: 2})
							}
						}
					}
				}()
			}

			// Unison (priority 1)
			if providers.richEnabled && providers.rich != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if fallbacks != nil {
						release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
						if !ok {
							return
						}
						defer release()
					}
					remote, err := providers.rich.Get(ctx, trackName, artistName, albumName)
					if err != nil || remote == nil || !validRichSyncType(remote.SyncType) {
						return
					}
					content, format, converted := compactRichSyncForStorage(remote.Content, remote.Format)
					if !converted {
						content, format = remote.Content, remote.Format
					}
					rich := db.RichLyrics{TrackID: trackID, Content: content, Format: format, SyncType: remote.SyncType, Source: remote.Source}
					if !isWordRichEmpty(&rich) {
						if err := db.UpsertRichLyrics(ctx, lyricsDB, rich); err == nil {
							updateBest(candidate{rich: &rich, priority: 1})
						}
					}
				}()
			}

			wg.Wait()
			if best.rich != nil {
				setRichOnlyResponse(&results[idx], best.rich)
			}
		}(i)
	}
	resultsWG.Wait()
	return nil
}
