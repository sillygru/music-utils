package metadata

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/names"
)

const (
	positiveCacheTTL    = time.Hour
	negativeCacheTTL    = 24 * time.Hour
	durationCacheStride = 2 // seconds
)

type cachedEntry struct {
	value     *db.Track
	notFound  bool
	expiresAt time.Time
}

type lookupCall struct {
	done  chan struct{}
	value *db.Track
	err   error
}

// Resolver chains providers in order and memoizes both positive results and
// not-found misses for a bounded lifetime so repeated lookups stop re-hitting
// upstream providers. inFlight coalesces concurrent misses for the same exact
// song, so a burst cannot spend one upstream request per caller.
type Resolver struct {
	providers []Provider
	mu        sync.Mutex
	cache     map[string]cachedEntry
	inFlight  map[string]*lookupCall
}

func NewResolver(providers ...Provider) *Resolver {
	return &Resolver{
		providers: providers,
		cache:     make(map[string]cachedEntry),
		inFlight:  make(map[string]*lookupCall),
	}
}

func cacheKey(input Input) string {
	input = normalizeInput(input)
	return normalize(input.TrackName) + "\x00" + normalize(input.ArtistName) + "\x00" + normalize(input.AlbumName) + "\x00" + durationKey(input.Duration)
}

// Search queries every provider that supports multi-result search and merges
// the responses in provider order. Duplicate tracks are kept once, while the
// first provider's metadata remains authoritative for the merged item.
func (r *Resolver) Search(ctx context.Context, query string, limit int) ([]*db.Track, error) {
	query = names.CleanSearch(query)
	if limit < 1 {
		return []*db.Track{}, nil
	}
	if limit > 50 {
		limit = 50
	}
	results := make([]*db.Track, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, provider := range r.providers {
		searchProvider, ok := provider.(SearchProvider)
		if !ok || searchProvider == nil {
			continue
		}
		tracks, err := searchProvider.Search(ctx, query, limit)
		if err != nil {
			continue
		}
		for _, track := range tracks {
			if track == nil {
				continue
			}
			key := normalize(track.Name) + "\x00" + normalize(track.ArtistName) + "\x00" + normalize(track.AlbumName)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			results = append(results, track)
		}
	}
	return results, nil
}

// Lookup resolves a track through the provider chain. A lookup that every
// provider refused because of upstream rate limiting returns an error that is
// both ErrNotFound (so existing callers keep treating it as "no result") and a
// RateLimitError (so callers that can pace themselves, such as the jobs
// backfill, can tell throttling apart from a genuine miss).
func (r *Resolver) Lookup(ctx context.Context, input Input) (*db.Track, error) {
	var inconclusive error
	for _, candidate := range inputCandidates(input) {
		track, err := r.lookupOne(ctx, candidate)
		if err == nil {
			return track, nil
		}
		if errors.Is(err, ErrInconclusive) {
			inconclusive = err
		}
	}
	// An inconclusive result is still reported as a miss, so callers that
	// answer HTTP requests behave exactly as before, but it is joined with
	// ErrInconclusive so anything that persists the outcome can tell the
	// difference between "this song has no match" and "we could not find out".
	if inconclusive != nil {
		return nil, inconclusive
	}
	return nil, ErrNotFound
}

func (r *Resolver) lookupOne(ctx context.Context, input Input) (*db.Track, error) {
	input = normalizeInput(input)
	key := cacheKey(input)
	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && time.Now().Before(entry.expiresAt) {
		r.mu.Unlock()
		if entry.notFound {
			return nil, ErrNotFound
		}
		return entry.value, nil
	}
	if call, ok := r.inFlight[key]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.value, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &lookupCall{done: make(chan struct{})}
	r.inFlight[key] = call
	r.mu.Unlock()

	var found *db.Track
	var throttled error
	var transient error
	for _, provider := range r.providers {
		if provider == nil {
			continue
		}
		track, err := provider.Lookup(ctx, input)
		if err == nil && track != nil && trackMatchesInput(input, track) {
			found = track
			break
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			// Transient/provider error without a result: keep trying the next
			// tier rather than recording a miss.
			if IsRateLimited(err) {
				throttled = err
			} else if transient == nil {
				transient = err
			}
			continue
		}
	}

	// Only an all-providers-agree-not-found result is memoized as a miss. A
	// throttle or a provider error proves nothing about whether the song exists,
	// so memoizing one would suppress every later retry for the negative-cache
	// TTL and let a track be recorded as permanently resolved on the strength of
	// a bad moment.
	resultErr := error(nil)
	switch {
	case found != nil:
		r.store(key, found, false)
	case throttled != nil:
		resultErr = errors.Join(ErrNotFound, ErrInconclusive, throttled)
	case transient != nil:
		resultErr = errors.Join(ErrNotFound, ErrInconclusive, transient)
	default:
		resultErr = ErrNotFound
		r.store(key, nil, true)
	}

	r.mu.Lock()
	call.value = found
	call.err = resultErr
	delete(r.inFlight, key)
	close(call.done)
	r.mu.Unlock()
	return found, resultErr
}

// Invalidate drops every memoized lookup, positive and negative alike.
//
// It exists for the background backfill job: the job writes resolved metadata
// straight to the database, and a miss memoized here for 24 hours would keep
// being served long after the job proved the track does exist. Clearing is
// cheap and safe, since in-flight lookups are tracked separately and are left
// to complete on their own.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache = make(map[string]cachedEntry)
}

func (r *Resolver) store(key string, track *db.Track, notFound bool) {
	ttl := negativeCacheTTL
	if !notFound {
		ttl = positiveCacheTTL
	}
	r.mu.Lock()
	r.cache[key] = cachedEntry{value: track, notFound: notFound, expiresAt: time.Now().Add(ttl)}
	r.mu.Unlock()
}

// trackMatchesInput prevents a provider from caching a result for a different
// song or artist merely because its search endpoint returned a non-empty row.
// Album names are intentionally not required to match: providers commonly
// return a canonical release variant for the same recording.
func trackMatchesInput(input Input, track *db.Track) bool {
	if track == nil {
		return false
	}
	actual := names.Normalize(track.Name, track.ArtistName, track.AlbumName)
	if normalize(input.TrackName) == "" || normalize(actual.TrackName) != normalize(input.TrackName) {
		return false
	}
	if normalize(input.ArtistName) != "" && normalize(actual.ArtistName) != normalize(input.ArtistName) {
		return false
	}
	return true
}

// durationKey collapses durations to a stride so provider second-level rounding
// does not split an identical track into separate lookup keys.
func durationKey(value float64) string {
	if value <= 0 {
		return "*"
	}
	return strconv.Itoa(int(value/durationCacheStride) * int(durationCacheStride))
}
