package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/names"
	"github.com/sillygru/music-utils/internal/richlyrics"
)

// runParallelLyricsGet starts every currently configured exact-lookup provider
// independently. The callback receives validated results as they arrive; all
// persistence happens inside the provider goroutines so results that arrive
// after the HTTP response are still cached. Slow providers can never delay the
// response: the orchestrator returns the best result so far after the
// three-second window while the remaining goroutines keep filling the cache.
// The skip set excludes providers already answered for this track (per the
// provider-fetch ledger); every outcome is written back to that ledger.
func runParallelLyricsGet(
	ctx context.Context,
	publish func(lyricsLookupResult),
	metadataDB, lyricsDB *sql.DB,
	providers *lyricsProviders,
	lyricsMisses *lyricsMissCache,
	fallbacks *fallbackGuard,
	richRequested bool,
	clientKey string,
	existingTrack *db.Track,
	trackName, artistName, albumName string,
	duration float64,
	videoID, isrc string,
	skip map[string]bool,
) {
	if providers == nil {
		return
	}
	if fallbacks != nil {
		release, status, retryAfter, ok := fallbacks.acquireFor(ctx, clientKey)
		if !ok {
			publish(lyricsLookupResult{err: &fallbackBlockedError{status: status, retryAfter: retryAfter}, status: status, retry: retryAfter})
			return
		}
		defer release()
	}
	var wg sync.WaitGroup
	if providers.lrclibEnabled && providers.lrclib != nil && !skip["lrclib"] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			remote, err := lookupRemoteLyricsBroadWithDuration(ctx, providers.lrclib, trackName, artistName, albumName, duration)
			elapsed := time.Since(started)
			if err != nil {
				if errors.Is(err, lrclib.ErrNotFound) && lyricsMisses != nil && artistName != "" {
					lyricsMisses.Set(lyricsMissKeyWithVideo(trackName, artistName, albumName, videoID), time.Now())
				}
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "lrclib", artistName, albumName)
				publish(lyricsLookupResult{err: err, upstream: elapsed})
				return
			}
			if !remoteLyricsAvailable(remote) || !remoteLyricsMatchesInput(names.Input{TrackName: trackName, ArtistName: artistName, AlbumName: albumName}, remote) {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "lrclib", artistName, albumName)
				publish(lyricsLookupResult{err: lrclib.ErrNotFound, upstream: elapsed})
				return
			}
			track, lyrics, ok := persistRemoteLyrics(ctx, metadataDB, lyricsDB, existingTrack, remote, trackName, artistName, albumName, duration)
			if !ok {
				return
			}
			recordProviderFetch(ctx, lyricsDB, track.ID, "lrclib", true)
			publish(lyricsLookupResult{track: track, lyrics: lyrics, upstream: elapsed})
		}()
	}

	if providers.richEnabled && richRequested && providers.rich != nil && !skip["unison"] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote, err := providers.rich.Get(ctx, trackName, artistName, albumName)
			if err != nil || !validRichSyncType(remote.SyncType) {
				if err != nil {
					recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "unison", artistName, albumName)
					publish(lyricsLookupResult{err: err})
				}
				return
			}
			track, rich, ok := persistRemoteRichLyrics(ctx, metadataDB, lyricsDB, existingTrack, remote, trackName, artistName, albumName, duration)
			if !ok {
				return
			}
			recordProviderFetch(ctx, lyricsDB, track.ID, "unison", true)
			publish(lyricsLookupResult{track: track, rich: rich})
		}()
	}

	if providers.appleEnabled && providers.apple != nil && !skip["apple_music"] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			track, err := providers.apple.SearchTrack(ctx, trackName, artistName, albumName)
			if err != nil {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "apple_music", artistName, albumName)
				return
			}
			remote, err := providers.apple.GetLyrics(ctx, track.ID)
			if err != nil {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "apple_music", artistName, albumName)
				return
			}
			trackRow, rich, ok := persistRemoteRichLyrics(ctx, metadataDB, lyricsDB, existingTrack, &richlyrics.Result{Content: remote.Content, Format: remote.Format, SyncType: remote.SyncType, Source: remote.Source}, track.Name, track.ArtistName, track.AlbumName, track.Duration)
			if ok {
				recordProviderFetch(ctx, lyricsDB, trackRow.ID, "apple_music", true)
				publish(lyricsLookupResult{track: trackRow, rich: rich})
			}
		}()
	}

	if providers.musixEnabled && providers.musix != nil && !skip["musixmatch"] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			track, err := providers.musix.SearchTrack(ctx, trackName, artistName, albumName)
			if err != nil {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "musixmatch", artistName, albumName)
				return
			}
			remote, err := providers.musix.GetLyrics(ctx, track.CommonTrackID, track.ISRC)
			if err != nil {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "musixmatch", artistName, albumName)
				return
			}
			row := &lrclib.RemoteResult{TrackName: track.Name, ArtistName: track.ArtistName, AlbumName: track.AlbumName, Duration: track.Duration, PlainLyrics: remote.PlainLyrics}
			trackRow, lyrics, ok := persistRemoteLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, track.Name, track.ArtistName, track.AlbumName, track.Duration)
			if ok {
				recordProviderFetch(ctx, lyricsDB, trackRow.ID, "musixmatch", true)
				publish(lyricsLookupResult{track: trackRow, lyrics: lyrics})
			}
		}()
	}

	if providers.betterEnabled && providers.better != nil && !skip["betterlyrics"] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			remote, err := providers.better.Get(ctx, trackName, artistName, albumName, duration)
			elapsed := time.Since(started)
			if err != nil {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "betterlyrics", artistName, albumName)
				return
			}
			row := &lrclib.RemoteResult{TrackName: trackName, ArtistName: artistName, AlbumName: albumName, Duration: duration, PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}
			trackRow, lyrics, ok := persistProviderLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, trackName, artistName, albumName, duration, "betterlyrics")
			if !ok {
				return
			}
			recordProviderFetch(ctx, lyricsDB, trackRow.ID, "betterlyrics", true)
			result := lyricsLookupResult{track: trackRow, lyrics: lyrics, upstream: elapsed}
			if remote.WordSynced && strings.TrimSpace(remote.TTML) != "" {
				if _, rich, richOK := persistRemoteRichLyrics(ctx, metadataDB, lyricsDB, trackRow, &richlyrics.Result{Content: remote.TTML, Format: "ttml", SyncType: "word", Source: "betterlyrics"}, trackName, artistName, albumName, duration); richOK {
					result.rich = rich
					// Ensure track points to the rich-persisted row.
					result.track = trackRow
				}
			}
			publish(result)
		}()
	}

	if providers.kugouEnabled && providers.kugou != nil && !skip["kugou"] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			remote, err := providers.kugou.Get(ctx, trackName, artistName, albumName, duration)
			elapsed := time.Since(started)
			if err != nil {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "kugou", artistName, albumName)
				return
			}
			row := &lrclib.RemoteResult{
				TrackName: firstNonEmpty(remote.TrackName, trackName), ArtistName: firstNonEmpty(remote.ArtistName, artistName),
				AlbumName: firstNonEmpty(remote.AlbumName, albumName), Duration: remote.Duration,
				PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
			}
			if row.Duration <= 0 {
				row.Duration = duration
			}
			trackRow, lyrics, ok := persistProviderLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, trackName, artistName, albumName, duration, "kugou")
			if ok {
				recordProviderFetch(ctx, lyricsDB, trackRow.ID, "kugou", true)
				publish(lyricsLookupResult{track: trackRow, lyrics: lyrics, upstream: elapsed})
			}
		}()
	}

	if providers.paxsenixEnabled && providers.paxsenix != nil && !skip["paxsenix"] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			remote, err := providers.paxsenix.Get(ctx, trackName, artistName, albumName)
			elapsed := time.Since(started)
			if err != nil {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "paxsenix", artistName, albumName)
				return
			}
			row := &lrclib.RemoteResult{
				TrackName: firstNonEmpty(remote.TrackName, trackName), ArtistName: firstNonEmpty(remote.ArtistName, artistName),
				AlbumName: firstNonEmpty(remote.AlbumName, albumName), Duration: remote.Duration,
				PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
			}
			if row.Duration <= 0 {
				row.Duration = duration
			}
			trackRow, lyrics, ok := persistProviderLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, trackName, artistName, albumName, duration, "paxsenix")
			if !ok {
				return
			}
			recordProviderFetch(ctx, lyricsDB, trackRow.ID, "paxsenix", true)
			result := lyricsLookupResult{track: trackRow, lyrics: lyrics, upstream: elapsed}
			if remote.WordSynced && strings.TrimSpace(remote.TTML) != "" {
				if _, rich, richOK := persistRemoteRichLyrics(ctx, metadataDB, lyricsDB, trackRow, &richlyrics.Result{Content: remote.TTML, Format: "ttml", SyncType: "word", Source: "paxsenix"}, trackName, artistName, albumName, duration); richOK {
					result.rich = rich
				}
			}
			publish(result)
		}()
	}

	if providers.lyricsPlusEnabled && providers.lyricsPlus != nil && !skip["lyricsplus"] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			remote, err := providers.lyricsPlus.Get(ctx, trackName, artistName, albumName, duration, isrc)
			elapsed := time.Since(started)
			if err != nil {
				recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "lyricsplus", artistName, albumName)
				return
			}
			row := &lrclib.RemoteResult{
				TrackName: firstNonEmpty(remote.TrackName, trackName), ArtistName: firstNonEmpty(remote.ArtistName, artistName),
				AlbumName: firstNonEmpty(remote.AlbumName, albumName), Duration: duration,
				PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
			}
			trackRow, lyrics, ok := persistProviderLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, trackName, artistName, albumName, duration, "lyricsplus")
			if !ok {
				return
			}
			recordProviderFetch(ctx, lyricsDB, trackRow.ID, "lyricsplus", true)
			result := lyricsLookupResult{track: trackRow, lyrics: lyrics, upstream: elapsed}
			if remote.WordSynced {
				if strings.TrimSpace(remote.TTML) != "" {
					if _, rich, richOK := persistRemoteRichLyrics(ctx, metadataDB, lyricsDB, trackRow, &richlyrics.Result{Content: remote.TTML, Format: "ttml", SyncType: "word", Source: "lyricsplus"}, trackName, artistName, albumName, duration); richOK {
						result.rich = rich
					}
				} else if strings.TrimSpace(remote.RichJSON) != "" {
					rich := &db.RichLyrics{TrackID: trackRow.ID, Content: remote.RichJSON, Format: "json", SyncType: "word", Source: "lyricsplus"}
					if err := db.UpsertRichLyrics(ctx, lyricsDB, *rich); err == nil {
						// Refresh rich with normalized hash.
						if stored, err := db.FindRichLyrics(ctx, lyricsDB, trackRow.ID, "word"); err == nil {
							result.rich = stored
						} else {
							result.rich = rich
						}
					}
				}
			}
			publish(result)
		}()
	}

	if videoID != "" {
		if providers.zemerEnabled && providers.zemer != nil && !skip["zemer"] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				started := time.Now()
				remote, err := providers.zemer.Get(ctx, videoID)
				elapsed := time.Since(started)
				if err != nil {
					recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "zemer", artistName, albumName)
					return
				}
				row := &lrclib.RemoteResult{TrackName: trackName, ArtistName: artistName, AlbumName: albumName, Duration: duration, PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}
				trackRow, lyrics, ok := persistProviderLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, trackName, artistName, albumName, duration, "zemer")
				if ok {
					recordProviderFetch(ctx, lyricsDB, trackRow.ID, "zemer", true)
					publish(lyricsLookupResult{track: trackRow, lyrics: lyrics, upstream: elapsed})
				}
			}()
		}

		if providers.tube != nil && providers.tubeLyricsEnabled && !skip["youtube"] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := providers.tube.GetOfficialLyrics(ctx, videoID)
				if err != nil {
					recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "youtube", artistName, albumName)
					return
				}
				row := &lrclib.RemoteResult{TrackName: trackName, ArtistName: artistName, AlbumName: albumName, Duration: duration, PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}
				trackRow, lyrics, ok := persistProviderLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, trackName, artistName, albumName, duration, "youtube")
				if ok {
					recordProviderFetch(ctx, lyricsDB, trackRow.ID, "youtube", true)
					publish(lyricsLookupResult{track: trackRow, lyrics: lyrics})
				}
			}()
		}

		if providers.tube != nil && providers.tubeSubtitleEnabled && !skip["youtube_subtitle"] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				remote, err := providers.tube.GetTranscript(ctx, videoID)
				if err != nil {
					recordProviderMiss(ctx, lyricsDB, trackIDOf(existingTrack), "youtube_subtitle", artistName, albumName)
					return
				}
				row := &lrclib.RemoteResult{TrackName: trackName, ArtistName: artistName, AlbumName: albumName, Duration: duration, PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics}
				trackRow, lyrics, ok := persistProviderLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, trackName, artistName, albumName, duration, "youtube_subtitle")
				if ok {
					recordProviderFetch(ctx, lyricsDB, trackRow.ID, "youtube_subtitle", true)
					publish(lyricsLookupResult{track: trackRow, lyrics: lyrics})
				}
			}()
		}
	}

	wg.Wait()
}

// trackIDOf returns the persisted row ID of an existing track, or zero when
// the track is not yet stored. Ledger writes for unpersisted tracks are
// dropped: the row may not exist to attach to.
func trackIDOf(track *db.Track) int64 {
	if track == nil {
		return 0
	}
	return track.ID
}

func lookupRemoteLyricsBroad(ctx context.Context, client *lrclib.Client, trackName, artistName, albumName string) (*lrclib.RemoteResult, error) {
	return lookupRemoteLyricsBroadWithDuration(ctx, client, trackName, artistName, albumName, 0)
}

func lookupRemoteLyricsBroadWithDuration(ctx context.Context, client *lrclib.Client, trackName, artistName, albumName string, duration float64) (*lrclib.RemoteResult, error) {
	remote, err := lookupRemoteLyricsWithDuration(ctx, client, trackName, artistName, albumName, duration)
	if err == nil || !errors.Is(err, lrclib.ErrNotFound) || artistName == "" || albumName == "" {
		return remote, err
	}
	results, searchErr := client.Search(ctx, strings.Join(nonEmpty(trackName, artistName), " "))
	if searchErr != nil {
		return nil, searchErr
	}
	if match := matchingLyricsResult(results, trackName, artistName); match != nil {
		return match, nil
	}
	return nil, lrclib.ErrNotFound
}

func persistRemoteLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB, existing *db.Track, remote *lrclib.RemoteResult, trackName, artistName, albumName string, duration float64) (*db.Track, *db.Lyrics, bool) {
	return persistProviderLyrics(ctx, metadataDB, lyricsDB, existing, remote, trackName, artistName, albumName, duration, "lrclib_fallback")
}

// persistProviderLyrics stores one provider's result under the requested
// identity, tagging both rows with the provider source.
func persistProviderLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB, existing *db.Track, remote *lrclib.RemoteResult, trackName, artistName, albumName string, duration float64, source string) (*db.Track, *db.Lyrics, bool) {
	if remote == nil {
		return nil, nil, false
	}
	track := db.Track{
		Name:       firstNonEmpty(remote.TrackName, trackName),
		ArtistName: firstNonEmpty(remote.ArtistName, artistName),
		AlbumName:  firstNonEmpty(remote.AlbumName, albumName),
		Duration:   remote.Duration,
		Source:     source,
	}
	if track.Duration <= 0 {
		track.Duration = duration
	}
	if existing != nil {
		track = *existing
		track.Source = source
	}
	trackID, _, err := db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, db.Lyrics{
		PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
		Instrumental: remote.Instrumental, Source: source,
	})
	if err != nil {
		return nil, nil, false
	}
	track.ID = trackID
	return &track, &db.Lyrics{PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics, Instrumental: remote.Instrumental}, true
}

func persistRichContent(ctx context.Context, metadataDB, lyricsDB *sql.DB, existing *db.Track, content, format, syncType, source, trackName, artistName, albumName string, duration float64) (*db.Track, *db.RichLyrics, bool) {
	remote := &richlyrics.Result{Content: content, Format: format, SyncType: syncType, Source: source}
	return persistRemoteRichLyrics(ctx, metadataDB, lyricsDB, existing, remote, trackName, artistName, albumName, duration)
}

func persistRemoteRichLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB, existing *db.Track, remote *richlyrics.Result, trackName, artistName, albumName string, duration float64) (*db.Track, *db.RichLyrics, bool) {
	if remote == nil || strings.TrimSpace(remote.Content) == "" {
		return nil, nil, false
	}
	track := db.Track{Name: trackName, ArtistName: artistName, AlbumName: albumName, Duration: duration, Source: "unison_rich_fallback"}
	if existing != nil {
		track = *existing
		track.Source = "unison_rich_fallback"
	}
	trackID := track.ID
	if trackID <= 0 {
		var err error
		trackID, _, err = db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, db.Lyrics{Source: "unison_rich_fallback"})
		if err != nil {
			return nil, nil, false
		}
	}
	content, format, converted := compactRichSyncForStorage(remote.Content, remote.Format)
	if !converted {
		content, format = remote.Content, remote.Format
	}
	rich := &db.RichLyrics{TrackID: trackID, Content: content, Format: format, SyncType: remote.SyncType, Source: remote.Source}
	if err := db.UpsertRichLyrics(ctx, lyricsDB, *rich); err != nil {
		return nil, nil, false
	}
	track.ID = trackID
	return &track, rich, true
}

func responseFromParallelLookup(result lyricsLookupResult, richRequested bool) (lyricsResponse, bool) {
	track := result.track
	if track == nil {
		return lyricsResponse{}, false
	}
	lyrics := result.lyrics
	rich := result.rich
	response := toLyricsResponse(track, lyrics)
	if richRequested && rich != nil {
		setRichOnlyResponse(&response, rich)
	}
	return response, response.RichSync != nil || lyricsAvailable(lyrics)
}
