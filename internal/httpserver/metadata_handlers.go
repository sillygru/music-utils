package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
	"github.com/sillygru/music-utils/internal/names"
)

type metadataResponse struct {
	ID                        int64   `json:"id"`
	TrackName                 string  `json:"trackName"`
	ArtistName                string  `json:"artistName"`
	AlbumName                 string  `json:"albumName"`
	Duration                  float64 `json:"duration"`
	Genre                     string  `json:"genre,omitempty"`
	Year                      int     `json:"year,omitempty"`
	ReleaseDate               string  `json:"releaseDate,omitempty"`
	ISRC                      string  `json:"isrc,omitempty"`
	MusicBrainzRecordingID    string  `json:"musicbrainzRecordingId,omitempty"`
	MusicBrainzReleaseID      string  `json:"musicbrainzReleaseId,omitempty"`
	MusicBrainzReleaseGroupID string  `json:"musicbrainzReleaseGroupId,omitempty"`
	MusicBrainzArtistID       string  `json:"musicbrainzArtistId,omitempty"`
	CoverURL                  string  `json:"coverUrl,omitempty"`
	MetadataSource            string  `json:"metadataSource,omitempty"`
	CoverURLSource            string  `json:"coverUrlSource,omitempty"`
}

func getMetadataHandler(database *sql.DB, resolver *metadata.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		candidates := names.Candidates(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"))
		input := candidates[0]
		name, album := input.TrackName, input.AlbumName
		if name == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "track_name is required"})
			return
		}
		duration, err := optionalDuration(query.Get("duration"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "duration must be a non-negative number"})
			return
		}
		cacheStart := time.Now()
		var local *db.Track
		for _, candidate := range candidates {
			local, err = db.FindTrackMetadataExact(r.Context(), database, candidate.TrackName, candidate.ArtistName, candidate.AlbumName, duration)
			if err == nil || !errors.Is(err, sql.ErrNoRows) {
				break
			}
		}
		setCacheDuration(r, time.Since(cacheStart))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		if err == nil && local.MetadataChecked {
			setOutcome(r, "local_hit")
			writeJSON(w, http.StatusOK, toMetadataResponse(local))
			return
		}
		if !fallbackEnabled || resolver == nil {
			if local != nil {
				setOutcome(r, "local_partial_hit")
				writeJSON(w, http.StatusOK, toMetadataResponse(local))
				return
			}
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
			return
		}

		release, ok := fallbacks.enter(r, w)
		if !ok {
			return
		}
		defer release()

		upstreamStart := time.Now()
		remote, err := resolver.Lookup(r.Context(), metadata.Input{TrackName: query.Get("track_name"), ArtistName: query.Get("artist_name"), AlbumName: query.Get("album_name"), Duration: duration})
		setUpstreamDuration(r, time.Since(upstreamStart))
		if err != nil {
			if !errors.Is(err, metadata.ErrNotFound) {
				setRequestIssue(r, slog.LevelWarn, err.Error())
			}
			if local != nil {
				setOutcome(r, "local_partial_hit")
				writeJSON(w, http.StatusOK, toMetadataResponse(local))
				return
			}
			setOutcome(r, "miss")
			writeJSON(w, http.StatusNotFound, apiError{Code: http.StatusNotFound, Message: "Track not found"})
			return
		}
		if local != nil && local.Duration > 0 {
			remote.Duration = local.Duration
		}
		if remote.AlbumName == "" {
			remote.AlbumName = album
		}
		remote.MetadataChecked = true
		trackID, err := db.UpsertTrackMetadata(r.Context(), database, *remote)
		if err != nil {
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		remote.ID = trackID
		setOutcome(r, "provider_fallback_hit")
		writeJSON(w, http.StatusOK, toMetadataResponse(remote))
	}
}

// metadataSearchWait bounds how long a search with nothing local to show waits
// on the upstream fan-out before answering. It matches the lyrics response
// window (lyricsResponseWait) so both endpoints behave the same way: a request
// that has something to return never waits at all, and one that would otherwise
// return an empty page gets a bounded wait rather than the full provider
// timeout.
const metadataSearchWait = lyricsResponseWait

// metadataSearchJob is one shared upstream fan-out for a search query.
type metadataSearchJob struct {
	done chan struct{}

	mu      sync.Mutex
	results []*db.Track
}

// publish records whatever the fan-out has resolved so far. It is called once,
// after the upstream search returns, but it is shaped as a callback so a future
// provider fan-out can stream partial results the way the lyrics path does.
func (j *metadataSearchJob) publish(results []*db.Track) {
	j.mu.Lock()
	if len(results) > len(j.results) {
		j.results = append([]*db.Track(nil), results...)
	}
	j.mu.Unlock()
}

func (j *metadataSearchJob) snapshot() []*db.Track {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]*db.Track(nil), j.results...)
}

// await returns the fan-out's results, giving up after wait or when the request
// ends. Returning whatever is available is deliberate: a late answer is still
// worth persisting, it just is not worth holding a response open for.
func (j *metadataSearchJob) await(ctx context.Context, wait time.Duration) []*db.Track {
	if results := j.snapshot(); len(results) > 0 {
		return results
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return j.snapshot()
}

// metadataSearchGroup guarantees one upstream fan-out per query, so a burst of
// identical searches spends one provider round trip rather than one per caller.
//
// The fan-out runs on a context detached from the request on purpose. A search
// that already has local rows to answer with returns immediately, and the fetch
// it triggers has to outlive that response to reach the database and profit the
// next request.
type metadataSearchGroup struct {
	mu   sync.Mutex
	jobs map[string]*metadataSearchJob
}

func newMetadataSearchGroup() *metadataSearchGroup {
	return &metadataSearchGroup{jobs: make(map[string]*metadataSearchJob)}
}

// join returns the shared job for key, starting the fan-out if this caller is
// the first to ask for it.
func (g *metadataSearchGroup) join(key string, start func(context.Context, func([]*db.Track))) *metadataSearchJob {
	g.mu.Lock()
	defer g.mu.Unlock()
	if job, ok := g.jobs[key]; ok {
		return job
	}
	job := &metadataSearchJob{done: make(chan struct{})}
	g.jobs[key] = job
	// The context is bounded so a fan-out that no provider ever answers cannot
	// outlive the process's patience for one query.
	jobCtx, cancel := context.WithTimeout(context.Background(), metadataSearchFanOutTimeout)
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
	return job
}

// metadataSearchFanOutTimeout bounds a single background search fan-out,
// independently of whichever request is waiting on it.
const metadataSearchFanOutTimeout = 30 * time.Second

// fillMetadataSearch runs the upstream half of a metadata search and persists
// every track it resolves.
//
// Persisting is what makes a background fill worth doing: without it the fetch
// only ever serves the request that triggered it, and the next identical
// search pays upstream again. Upserting also stamps metadata_checked, so the
// per-track /api/metadata/get lookup for a resolved track becomes a local hit.
func fillMetadataSearch(ctx context.Context, publish func([]*db.Track), database *sql.DB, resolver *metadata.Resolver, fallbacks *fallbackGuard, clientKey, searchQuery string, limit int) {
	release, _, _, ok := fallbacks.acquireFor(ctx, clientKey)
	if !ok {
		return
	}
	defer release()
	remote, err := resolver.Search(ctx, searchQuery, limit)
	if err != nil {
		return
	}
	for _, track := range remote {
		if track == nil {
			continue
		}
		// A search result is a definitive provider answer, so it settles the
		// track rather than leaving it for the backfill job to ask again.
		track.MetadataChecked = true
		if id, persistErr := db.UpsertTrackMetadata(ctx, database, *track); persistErr == nil {
			track.ID = id
		}
	}
	publish(remote)
}

func searchMetadataHandlerWithUpstream(database *sql.DB, resolver *metadata.Resolver, fallbacks *fallbackGuard, fallbackEnabled bool) http.HandlerFunc {
	group := newMetadataSearchGroup()
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		searchQuery := names.CleanSearch(query.Get("q"))
		if searchQuery == "" {
			searchQuery = names.CleanSearch(strings.Join(nonEmpty(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"), query.Get("genre")), " "))
		}
		if searchQuery == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "q or track_name, artist_name, album_name, or genre is required"})
			return
		}
		limit, err := searchLimit(query.Get("limit"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
			return
		}
		cacheStart := time.Now()
		tracks, err := db.SearchTracks(r.Context(), database, nil, searchQuery, limit)
		if err != nil {
			setCacheDuration(r, time.Since(cacheStart))
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		results := make([]metadataResponse, 0, limit)
		seen := make(map[string]struct{}, limit)
		appendTrack := func(track *db.Track) {
			if track == nil || len(results) >= limit {
				return
			}
			key := strings.ToLower(strings.TrimSpace(track.Name)) + "\x00" + strings.ToLower(strings.TrimSpace(track.ArtistName)) + "\x00" + strings.ToLower(strings.TrimSpace(track.AlbumName))
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
			results = append(results, toMetadataResponse(track))
		}
		// Strict FTS rows match only when every query token is present, so they
		// outrank a provider's fuzzy hits. Appending them first also keeps a
		// full page of loosely related provider results from crowding them out
		// of the limit entirely.
		for i := range tracks {
			appendTrack(&tracks[i].Track)
		}
		// Measured before the relaxed pass. The upstream fan-out below is
		// triggered by a cache that had nothing to show, so growing the local
		// recall must not turn into extra provider traffic: a query the relaxed
		// pass fills is a cache hit, not a miss.
		strictCount := len(results)

		// A query carrying a word the catalog does not hold — a typo, an
		// abbreviation — gets nothing from the strict match and would otherwise
		// cost a provider round trip for an answer that is already cached under
		// a slightly different spelling. Relaxing the match keeps that a local
		// read; the coverage filter inside SearchTracksRelaxed keeps it from
		// handing back rows the strict match was right to reject.
		if strictCount < limit {
			relaxed, relaxedErr := db.SearchTracksRelaxed(r.Context(), database, nil, searchQuery, limit)
			setCacheDuration(r, time.Since(cacheStart))
			if relaxedErr != nil {
				// The strict read already succeeded, so this is a degraded page
				// rather than a failed request: report it and serve what the
				// cache gave us.
				setRequestIssue(r, slog.LevelWarn, relaxedErr.Error())
			}
			for i := range relaxed {
				appendTrack(&relaxed[i].Track)
			}
		} else {
			setCacheDuration(r, time.Since(cacheStart))
		}
		hadLocal := len(results) > 0

		// A local page that already fills the limit is the whole answer. Asking a
		// provider anyway is how a search for a well-known track ends up waiting
		// on the upstream pacer queue to be told nothing it did not already have.
		if strictCount < limit && fallbackEnabled && resolver != nil && fallbacks != nil {
			clientKey := clientIP(r, false)
			job := group.join(searchQuery+"\x00"+strconv.Itoa(limit), func(ctx context.Context, publish func([]*db.Track)) {
				fillMetadataSearch(ctx, publish, database, resolver, fallbacks, clientKey, searchQuery, limit)
			})
			// Only a search with nothing to show waits. One that already has
			// local rows answers now and lets the fan-out persist for the next
			// caller, which is the whole point of running it detached.
			if len(results) == 0 {
				upstreamStart := time.Now()
				for _, track := range job.await(r.Context(), metadataSearchWait) {
					appendTrack(track)
				}
				setUpstreamDuration(r, time.Since(upstreamStart))
			}
		}
		if len(results) == 0 {
			setOutcome(r, "miss")
		} else if hadLocal {
			setOutcome(r, "local_hit")
		} else {
			setOutcome(r, "provider_fallback_hit")
		}
		writeJSON(w, http.StatusOK, results)
	}
}

func searchMetadataHandler(database *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		searchQuery := names.CleanSearch(query.Get("q"))
		if searchQuery == "" {
			searchQuery = names.CleanSearch(strings.Join(nonEmpty(query.Get("track_name"), query.Get("artist_name"), query.Get("album_name"), query.Get("genre")), " "))
		}
		if searchQuery == "" {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "q or track_name, artist_name, album_name, or genre is required"})
			return
		}
		limit, err := searchLimit(query.Get("limit"))
		if err != nil {
			setOutcome(r, "bad_request")
			writeJSON(w, http.StatusBadRequest, apiError{Code: http.StatusBadRequest, Message: "limit must be an integer between 1 and 50"})
			return
		}
		cacheStart := time.Now()
		tracks, err := db.SearchTracks(r.Context(), database, nil, searchQuery, limit)
		setCacheDuration(r, time.Since(cacheStart))
		if err != nil {
			setRequestIssue(r, slog.LevelError, err.Error())
			setOutcome(r, "error")
			writeJSON(w, http.StatusInternalServerError, apiError{Code: http.StatusInternalServerError, Message: "Internal server error"})
			return
		}
		result := make([]metadataResponse, 0, len(tracks))
		for i := range tracks {
			result = append(result, toMetadataResponse(&tracks[i].Track))
		}
		if len(result) == 0 {
			setOutcome(r, "miss")
		} else {
			setOutcome(r, "local_hit")
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func toMetadataResponse(track *db.Track) metadataResponse {
	return metadataResponse{
		ID: track.ID, TrackName: track.Name, ArtistName: track.ArtistName, AlbumName: track.AlbumName,
		Duration: track.Duration, Genre: track.Genre, Year: track.Year, ReleaseDate: track.ReleaseDate,
		ISRC: track.ISRC, MusicBrainzRecordingID: track.MusicBrainzRecordingID,
		MusicBrainzReleaseID: track.MusicBrainzReleaseID, MusicBrainzReleaseGroupID: track.MusicBrainzReleaseGroupID,
		MusicBrainzArtistID: track.MusicBrainzArtistID, CoverURL: track.CoverURL,
		MetadataSource: track.MetadataSource, CoverURLSource: track.CoverURLSource,
	}
}

func optionalDuration(value string) (float64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	duration, err := strconv.ParseFloat(value, 64)
	if err != nil || duration < 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return 0, errors.New("invalid duration")
	}
	return duration, nil
}
