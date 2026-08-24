package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/applemusic"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/musixmatch"
	"github.com/sillygru/music-utils/internal/names"
	"github.com/sillygru/music-utils/internal/richlyrics"
)

// runParallelLyricsGet starts every currently configured exact-lookup provider
// independently. The callback receives validated results as they arrive; all
// persistence happens inside the provider goroutines so results that arrive
// after the HTTP response are still cached.
func runParallelLyricsGet(
	ctx context.Context,
	publish func(lyricsLookupResult),
	metadataDB, lyricsDB *sql.DB,
	client *lrclib.Client,
	richClient *richlyrics.Client,
	appleClient *applemusic.Client,
	musixClient *musixmatch.Client,
	lyricsMisses *lyricsMissCache,
	fallbacks *fallbackGuard,
	fallbackEnabled, richEnabled, appleEnabled, musixEnabled, richRequested bool,
	clientKey string,
	existingTrack *db.Track,
	trackName, artistName, albumName string,
	duration float64,
) {
	var wg sync.WaitGroup
	if fallbackEnabled && client != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if fallbacks != nil {
				release, status, retryAfter, ok := fallbacks.acquireFor(ctx, clientKey)
				if !ok {
					publish(lyricsLookupResult{err: &fallbackBlockedError{status: status, retryAfter: retryAfter}, status: status, retry: retryAfter})
					return
				}
				defer release()
			}
			started := time.Now()
			remote, err := lookupRemoteLyricsBroad(ctx, client, trackName, artistName, albumName, duration)
			elapsed := time.Since(started)
			if err != nil {
				if errors.Is(err, lrclib.ErrNotFound) && lyricsMisses != nil && artistName != "" {
					lyricsMisses.Set(lyricsMissKey(trackName, artistName, albumName, duration), time.Now())
				}
				publish(lyricsLookupResult{err: err, upstream: elapsed})
				return
			}
			if !remoteLyricsAvailable(remote) || !remoteLyricsMatchesInput(names.Input{TrackName: trackName, ArtistName: artistName, AlbumName: albumName}, remote) {
				publish(lyricsLookupResult{err: lrclib.ErrNotFound, upstream: elapsed})
				return
			}
			track, lyrics, ok := persistRemoteLyrics(ctx, metadataDB, lyricsDB, existingTrack, remote, trackName, artistName, albumName, duration)
			if !ok {
				return
			}
			publish(lyricsLookupResult{track: track, lyrics: lyrics, upstream: elapsed})
		}()
	}

	if richEnabled && richRequested && richClient != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if fallbacks != nil {
				release, status, retryAfter, ok := fallbacks.acquireFor(ctx, clientKey)
				if !ok {
					publish(lyricsLookupResult{err: &fallbackBlockedError{status: status, retryAfter: retryAfter}, status: status, retry: retryAfter})
					return
				}
				defer release()
			}
			remote, err := richClient.Get(ctx, trackName, artistName, albumName, duration)
			if err != nil || !validRichSyncType(remote.SyncType) {
				if err != nil {
					publish(lyricsLookupResult{err: err})
				}
				return
			}
			track, rich, ok := persistRemoteRichLyrics(ctx, metadataDB, lyricsDB, existingTrack, remote, trackName, artistName, albumName, duration)
			if !ok {
				return
			}
			publish(lyricsLookupResult{track: track, rich: rich})
		}()
	}

	if appleEnabled && appleClient != nil {
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
			track, err := appleClient.SearchTrack(ctx, trackName, artistName, albumName, duration)
			if err != nil {
				return
			}
			remote, err := appleClient.GetLyrics(ctx, track.ID)
			if err != nil {
				return
			}
			trackRow, rich, ok := persistRemoteRichLyrics(ctx, metadataDB, lyricsDB, existingTrack, &richlyrics.Result{Content: remote.Content, Format: remote.Format, SyncType: remote.SyncType, Source: remote.Source}, track.Name, track.ArtistName, track.AlbumName, track.Duration)
			if ok {
				publish(lyricsLookupResult{track: trackRow, rich: rich})
			}
		}()
	}

	if musixEnabled && musixClient != nil {
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
			track, err := musixClient.SearchTrack(ctx, trackName, artistName, albumName, duration)
			if err != nil {
				return
			}
			remote, err := musixClient.GetLyrics(ctx, track.CommonTrackID, track.ISRC)
			if err != nil {
				return
			}
			row := &lrclib.RemoteResult{TrackName: track.Name, ArtistName: track.ArtistName, AlbumName: track.AlbumName, Duration: track.Duration, PlainLyrics: remote.PlainLyrics}
			trackRow, lyrics, ok := persistRemoteLyrics(ctx, metadataDB, lyricsDB, existingTrack, row, track.Name, track.ArtistName, track.AlbumName, track.Duration)
			if ok {
				publish(lyricsLookupResult{track: trackRow, lyrics: lyrics})
			}
		}()
	}

	wg.Wait()
}

func lookupRemoteLyricsBroad(ctx context.Context, client *lrclib.Client, trackName, artistName, albumName string, duration float64) (*lrclib.RemoteResult, error) {
	remote, err := lookupRemoteLyrics(ctx, client, trackName, artistName, albumName, duration)
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
	if remote == nil {
		return nil, nil, false
	}
	track := db.Track{
		Name:       firstNonEmpty(remote.TrackName, trackName),
		ArtistName: firstNonEmpty(remote.ArtistName, artistName),
		AlbumName:  firstNonEmpty(remote.AlbumName, albumName),
		Duration:   remote.Duration,
		Source:     "lrclib_fallback",
	}
	if track.Duration <= 0 {
		track.Duration = duration
	}
	if existing != nil {
		track = *existing
		track.Source = "lrclib_fallback"
	}
	trackID, _, err := db.InsertTrackWithLyrics(ctx, metadataDB, lyricsDB, track, db.Lyrics{
		PlainLyrics: remote.PlainLyrics, SyncedLyrics: remote.SyncedLyrics,
		Instrumental: remote.Instrumental, Source: "lrclib_fallback",
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
