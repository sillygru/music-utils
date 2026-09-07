package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
)

type lyricsSearchJob struct {
	done chan struct{}
	wake chan struct{}

	mu      sync.Mutex
	results []lyricsResponse
	cancel  context.CancelFunc
}

func (j *lyricsSearchJob) publish(results []lyricsResponse) {
	if len(results) == 0 {
		return
	}
	j.mu.Lock()
	j.results = results
	j.mu.Unlock()
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

func (j *lyricsSearchJob) snapshot() []lyricsResponse {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]lyricsResponse(nil), j.results...)
}

type lyricsSearchGroup struct {
	mu   sync.Mutex
	jobs map[string]*lyricsSearchJob
}

func newLyricsSearchGroup() *lyricsSearchGroup {
	return &lyricsSearchGroup{jobs: make(map[string]*lyricsSearchJob)}
}

// lookup waits for all results that arrive before the deadline. Unlike exact
// lookup it does not return on the first result because search should combine
// local and upstream results whenever they finish within the three-second UX
// window.
func (g *lyricsSearchGroup) lookup(ctx context.Context, key string, start func(context.Context, func([]lyricsResponse))) []lyricsResponse {
	g.mu.Lock()
	job, exists := g.jobs[key]
	if !exists {
		jobCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		job = &lyricsSearchJob{done: make(chan struct{}), wake: make(chan struct{}, 1), cancel: cancel}
		g.jobs[key] = job
		go func() {
			defer close(job.done)
			defer cancel()
			defer func() {
				g.mu.Lock()
				delete(g.jobs, key)
				g.mu.Unlock()
			}()
			start(jobCtx, job.publish)
		}()
	}
	g.mu.Unlock()

	timer := time.NewTimer(lyricsResponseWait)
	defer timer.Stop()
	for {
		select {
		case <-job.wake:
			continue
		case <-job.done:
			return job.snapshot()
		case <-timer.C:
			return job.snapshot()
		case <-ctx.Done():
			return job.snapshot()
		}
	}
}

func (g *lyricsSearchGroup) stop() {
	g.mu.Lock()
	jobs := make([]*lyricsSearchJob, 0, len(g.jobs))
	for _, job := range g.jobs {
		jobs = append(jobs, job)
	}
	g.mu.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
}

func runParallelLyricsSearch(
	ctx context.Context,
	publish func([]lyricsResponse),
	metadataDB, lyricsDB *sql.DB,
	providers *lyricsProviders,
	fallbacks *fallbackGuard,
	richRequested, skipRemote bool,
	clientKey, query string,
	hintTrack, hintArtist, hintAlbum, videoID string,
	limit int,
	cacheKey string,
) {
	var ordinaryWG sync.WaitGroup
	var mu sync.Mutex
	results := make([]lyricsResponse, 0, limit)

	merge := func(incoming []lyricsResponse) {
		mu.Lock()
		defer mu.Unlock()
		seen := make(map[string]struct{}, len(results)+len(incoming))
		for _, result := range results {
			seen[searchLyricsIdentity(result.TrackName, result.ArtistName)] = struct{}{}
		}
		for _, result := range incoming {
			identity := searchLyricsIdentity(result.TrackName, result.ArtistName)
			if index, ok := findLyricsResponse(results, identity); ok {
				mergeSearchResponse(&results[index], &result)
				continue
			}
			if _, ok := seen[identity]; ok || len(results) >= limit {
				continue
			}
			seen[identity] = struct{}{}
			results = append(results, result)
		}
		sort.SliceStable(results, func(i, j int) bool {
			return searchResponseScore(results[i]) > searchResponseScore(results[j])
		})
		if len(results) > limit {
			results = results[:limit]
		}
		if encoded, err := json.Marshal(results); err == nil {
			_ = db.UpsertLyricsSearchCache(context.Background(), lyricsDB, cacheKey, encoded)
		}
		publish(append([]lyricsResponse(nil), results...))
	}

	ordinaryWG.Add(1)
	go func() {
		defer ordinaryWG.Done()
		tracks, err := db.SearchTracks(ctx, metadataDB, lyricsDB, query, limit)
		if err != nil {
			return
		}
		local := make([]lyricsResponse, 0, len(tracks))
		for i := range tracks {
			response := toLyricsResponse(&tracks[i].Track, &tracks[i].Lyrics)
			appendLyricsVariant(&response, &response)
			local = append(local, response)
		}
		merge(local)
	}()

	if providers.lrclibEnabled && providers.lrclib != nil && !skipRemote {
		ordinaryWG.Add(1)
		go func() {
			defer ordinaryWG.Done()
			if fallbacks != nil {
				release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
				if !ok {
					return
				}
				defer release()
			}
			remote, err := providers.lrclib.Search(ctx, query)
			if err != nil {
				return
			}
			remoteResults := make([]lyricsResponse, 0, len(remote))
			for _, result := range remote {
				if synthesizedLyricsResult(result) || !remoteLyricsAvailable(&result) {
					continue
				}
				remoteResults = append(remoteResults, searchPersistResponse(ctx, metadataDB, lyricsDB, result, "lrclib_fallback"))
			}
			merge(remoteResults)
		}()
	}

	// Metadata-matched providers fan out on the structured hints (or the
	// free-text query as title). Each persists its own hits, which are merged
	// into the same ranked set.
	title := searchProviderTitle(hintTrack, query)
	if title != "" && !skipRemote {
		if providers.betterEnabled && providers.better != nil && hintArtist != "" {
			ordinaryWG.Add(1)
			go func() {
				defer ordinaryWG.Done()
				if fallbacks != nil {
					release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
					if !ok {
						return
					}
					defer release()
				}
				remote, err := providers.better.Get(ctx, title, hintArtist, hintAlbum, 0)
				if err != nil {
					return
				}
				resp := searchPersistResponse(ctx, metadataDB, lyricsDB, lrclib.RemoteResult{
					TrackName: title, ArtistName: hintArtist, AlbumName: hintAlbum,
					PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
				}, "betterlyrics")
				if remote.WordSynced && strings.TrimSpace(remote.TTML) != "" && resp.ID > 0 {
					content, format, converted := compactRichSyncForStorage(remote.TTML, "ttml")
					if !converted {
						content, format = remote.TTML, "ttml"
					}
					rich := db.RichLyrics{TrackID: resp.ID, Content: content, Format: format, SyncType: "word", Source: "betterlyrics"}
					if err := db.UpsertRichLyrics(ctx, lyricsDB, rich); err == nil {
						if stored, err := db.FindRichLyrics(ctx, lyricsDB, resp.ID, "word"); err == nil {
							setRichOnlyResponse(&resp, stored)
						} else {
							setRichOnlyResponse(&resp, &rich)
						}
					}
				}
				merge([]lyricsResponse{resp})
			}()
		}
		if providers.kugouEnabled && providers.kugou != nil {
			ordinaryWG.Add(1)
			go func() {
				defer ordinaryWG.Done()
				if fallbacks != nil {
					release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
					if !ok {
						return
					}
					defer release()
				}
				remote, err := providers.kugou.Get(ctx, title, hintArtist, hintAlbum, 0)
				if err != nil {
					return
				}
				merge([]lyricsResponse{searchPersistResponse(ctx, metadataDB, lyricsDB, lrclib.RemoteResult{
					TrackName: firstNonEmpty(remote.TrackName, title), ArtistName: firstNonEmpty(remote.ArtistName, hintArtist),
					AlbumName: firstNonEmpty(remote.AlbumName, hintAlbum), Duration: remote.Duration,
					PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
				}, "kugou")})
			}()
		}
		if providers.paxsenixEnabled && providers.paxsenix != nil {
			ordinaryWG.Add(1)
			go func() {
				defer ordinaryWG.Done()
				if fallbacks != nil {
					release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
					if !ok {
						return
					}
					defer release()
				}
				remote, err := providers.paxsenix.Get(ctx, title, hintArtist, hintAlbum)
				if err != nil {
					return
				}
				resp := searchPersistResponse(ctx, metadataDB, lyricsDB, lrclib.RemoteResult{
					TrackName: firstNonEmpty(remote.TrackName, title), ArtistName: firstNonEmpty(remote.ArtistName, hintArtist),
					AlbumName: firstNonEmpty(remote.AlbumName, hintAlbum), Duration: remote.Duration,
					PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
				}, "paxsenix")
				if remote.WordSynced && strings.TrimSpace(remote.TTML) != "" && resp.ID > 0 {
					content, format, converted := compactRichSyncForStorage(remote.TTML, "ttml")
					if !converted {
						content, format = remote.TTML, "ttml"
					}
					rich := db.RichLyrics{TrackID: resp.ID, Content: content, Format: format, SyncType: "word", Source: "paxsenix"}
					if err := db.UpsertRichLyrics(ctx, lyricsDB, rich); err == nil {
						if stored, err := db.FindRichLyrics(ctx, lyricsDB, resp.ID, "word"); err == nil {
							setRichOnlyResponse(&resp, stored)
						} else {
							setRichOnlyResponse(&resp, &rich)
						}
					}
				}
				merge([]lyricsResponse{resp})
			}()
		}
		if providers.lyricsPlusEnabled && providers.lyricsPlus != nil {
			ordinaryWG.Add(1)
			go func() {
				defer ordinaryWG.Done()
				if fallbacks != nil {
					release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
					if !ok {
						return
					}
					defer release()
				}
				remote, err := providers.lyricsPlus.Get(ctx, title, hintArtist, hintAlbum, 0, "")
				if err != nil {
					return
				}
				resp := searchPersistResponse(ctx, metadataDB, lyricsDB, lrclib.RemoteResult{
					TrackName: firstNonEmpty(remote.TrackName, title), ArtistName: firstNonEmpty(remote.ArtistName, hintArtist),
					AlbumName: firstNonEmpty(remote.AlbumName, hintAlbum),
					PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
				}, "lyricsplus")
				if remote.WordSynced && resp.ID > 0 {
					if strings.TrimSpace(remote.TTML) != "" {
						content, format, converted := compactRichSyncForStorage(remote.TTML, "ttml")
						if !converted {
							content, format = remote.TTML, "ttml"
						}
						rich := db.RichLyrics{TrackID: resp.ID, Content: content, Format: format, SyncType: "word", Source: "lyricsplus"}
						if err := db.UpsertRichLyrics(ctx, lyricsDB, rich); err == nil {
							if stored, err := db.FindRichLyrics(ctx, lyricsDB, resp.ID, "word"); err == nil {
								setRichOnlyResponse(&resp, stored)
							} else {
								setRichOnlyResponse(&resp, &rich)
							}
						}
					} else if strings.TrimSpace(remote.RichJSON) != "" {
						rich := db.RichLyrics{TrackID: resp.ID, Content: remote.RichJSON, Format: "json", SyncType: "word", Source: "lyricsplus"}
						if err := db.UpsertRichLyrics(ctx, lyricsDB, rich); err == nil {
							if stored, err := db.FindRichLyrics(ctx, lyricsDB, resp.ID, "word"); err == nil {
								setRichOnlyResponse(&resp, stored)
							} else {
								setRichOnlyResponse(&resp, &rich)
							}
						}
					}
				}
				merge([]lyricsResponse{resp})
			}()
		}
	}

	// Video-keyed providers resolve exactly one video and merge it when found.
	if videoID != "" && !skipRemote {
		if providers.zemerEnabled && providers.zemer != nil {
			ordinaryWG.Add(1)
			go func() {
				defer ordinaryWG.Done()
				if fallbacks != nil {
					release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
					if !ok {
						return
					}
					defer release()
				}
				remote, err := providers.zemer.Get(ctx, videoID)
				if err != nil {
					return
				}
				merge([]lyricsResponse{searchPersistResponse(ctx, metadataDB, lyricsDB, lrclib.RemoteResult{
					TrackName: title, ArtistName: hintArtist, AlbumName: hintAlbum,
					PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
				}, "zemer")})
			}()
		}
		if providers.tube != nil && (providers.tubeLyricsEnabled || providers.tubeSubtitleEnabled) {
			ordinaryWG.Add(1)
			go func() {
				defer ordinaryWG.Done()
				if fallbacks != nil {
					release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
					if !ok {
						return
					}
					defer release()
				}
				if providers.tubeLyricsEnabled {
					if remote, err := providers.tube.GetOfficialLyrics(ctx, videoID); err == nil {
						merge([]lyricsResponse{searchPersistResponse(ctx, metadataDB, lyricsDB, lrclib.RemoteResult{
							TrackName: title, ArtistName: hintArtist, AlbumName: hintAlbum,
							PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
						}, "youtube")})
					}
				}
				if providers.tubeSubtitleEnabled {
					if remote, err := providers.tube.GetTranscript(ctx, videoID); err == nil {
						merge([]lyricsResponse{searchPersistResponse(ctx, metadataDB, lyricsDB, lrclib.RemoteResult{
							TrackName: title, ArtistName: hintArtist, AlbumName: hintAlbum,
							PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
						}, "youtube_subtitle")})
					}
				}
			}()
		}
	}

	ordinaryWG.Wait()
	if providers.richEnabled && richRequested && providers.rich != nil {
		var richWG sync.WaitGroup
		mu.Lock()
		current := append([]lyricsResponse(nil), results...)
		mu.Unlock()
		for i := range current {
			if current[i].ID <= 0 {
				continue
			}
			richWG.Add(1)
			go func(index int) {
				defer richWG.Done()
				if fallbacks != nil {
					release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
					if !ok {
						return
					}
					defer release()
				}
				remote, err := providers.rich.Get(ctx, current[index].TrackName, current[index].ArtistName, current[index].AlbumName)
				if err != nil || !validRichSyncType(remote.SyncType) {
					return
				}
				content, format, converted := compactRichSyncForStorage(remote.Content, remote.Format)
				if !converted {
					content, format = remote.Content, remote.Format
				}
				rich := db.RichLyrics{TrackID: current[index].ID, Content: content, Format: format, SyncType: remote.SyncType, Source: remote.Source}
				if err := db.UpsertRichLyrics(ctx, lyricsDB, rich); err != nil {
					return
				}
				setRichOnlyResponse(&current[index], &rich)
			}(i)
		}
		richWG.Wait()
		merge(current)
	}
}

func findLyricsResponse(results []lyricsResponse, identity string) (int, bool) {
	for i := range results {
		if searchLyricsIdentity(results[i].TrackName, results[i].ArtistName) == identity {
			return i, true
		}
	}
	return 0, false
}

func mergeSearchResponse(existing, incoming *lyricsResponse) {
	if existing == nil || incoming == nil {
		return
	}
	mergeLyricsVariants(existing, incoming)
	if incoming.RichSync != nil {
		existing.RichSync = incoming.RichSync
		appendLyricsVariant(existing, incoming)
		existing.PlainLyrics = ""
		existing.SyncedLyrics = ""
		return
	}
	if existing.PlainLyrics == "" {
		existing.PlainLyrics = incoming.PlainLyrics
	}
	if existing.SyncedLyrics == "" {
		existing.SyncedLyrics = incoming.SyncedLyrics
	}
	if existing.Instrumental {
		return
	}
	existing.Instrumental = incoming.Instrumental
}

func searchResponseScore(result lyricsResponse) int {
	if result.RichSync != nil {
		return 3
	}
	if result.SyncedLyrics != "" {
		return 2
	}
	if result.PlainLyrics != "" || result.Instrumental {
		return 1
	}
	return 0
}

func canonicalLyricsSearchKey(query string, limit int, includeRich bool, syncType string) string {
	return strings.ToLower(strings.TrimSpace(query)) + "\x00" + strconv.Itoa(limit) + "\x00" + strconv.FormatBool(includeRich) + "\x00" + strings.ToLower(strings.TrimSpace(syncType))
}
