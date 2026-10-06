package cover

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned when no provider can resolve artwork.
var ErrNotFound = errors.New("cover not found in any provider")

const (
	positiveCacheTTL = time.Hour
	negativeCacheTTL = 24 * time.Hour
)

type cacheEntry struct {
	result    *Result
	persisted bool // set by handlers, not by the resolver
	notFound  bool
	expiresAt time.Time
}

// Resolver chains providers in order and memoizes both positive results and
// not-found misses so repeated lookups stop re-hitting upstream sources.
//
// inFlight coalesces concurrent work for one key. The memo maps answer only
// once a lookup has finished, so without it a burst of identical requests all
// miss together and each runs its own provider fan-out. That costs twice over:
// the shared upstream pacers space those duplicates apart, so the last caller
// in a burst waits behind every other one, and the providers are asked the same
// question once per caller.
type Resolver struct {
	providers      []Provider
	mu             sync.Mutex
	cache          map[string]cacheEntry
	search         map[string]searchCacheEntry
	inFlight       map[string]*searchCall
	lookupInFlight map[string]*lookupCall
}

type searchCacheEntry struct {
	results   []Result
	expiresAt time.Time
}

// searchCall is one shared in-progress provider fan-out. Waiters block on done
// and then read the leader's result, so the provider work happens once per key
// no matter how many callers arrive together.
type searchCall struct {
	done    chan struct{}
	results []Result
	err     error
}

// lookupCall is the same idea for an exact lookup, which resolves to one result
// rather than a list.
type lookupCall struct {
	done   chan struct{}
	result *Result
	err    error
}

// NewResolver builds a resolver over the given providers (nil entries are
// skipped). Order is the fallback order.
func NewResolver(providers ...Provider) *Resolver {
	return &Resolver{
		providers:      providers,
		cache:          make(map[string]cacheEntry),
		search:         make(map[string]searchCacheEntry),
		inFlight:       make(map[string]*searchCall),
		lookupInFlight: make(map[string]*lookupCall),
	}
}

func cacheKey(kind Kind, input Input) string {
	input = normalizeInput(input)
	return kind.String() + "\x00" + normalize(input.TrackName) + "\x00" + normalize(input.ArtistName) + "\x00" + normalize(input.AlbumName)
}

// Search asks every configured provider for its top result and returns those
// results in provider order. Unlike Lookup, it intentionally does not stop at
// the first provider so callers can show provenance from multiple APIs.
func (r *Resolver) Search(ctx context.Context, kind Kind, input Input, limit int) ([]Result, error) {
	input = normalizeInput(input)
	if limit < 1 {
		return []Result{}, nil
	}
	key := cacheKey(kind, input)
	r.mu.Lock()
	if entry, ok := r.search[key]; ok && time.Now().Before(entry.expiresAt) {
		results := append([]Result(nil), entry.results...)
		r.mu.Unlock()
		if len(results) > limit {
			results = results[:limit]
		}
		return results, nil
	}
	// A caller that arrives while the fan-out is already running waits for it
	// rather than starting a second one.
	if call, ok := r.inFlight[key]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			results := append([]Result(nil), call.results...)
			if len(results) > limit {
				results = results[:limit]
			}
			return results, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &searchCall{done: make(chan struct{})}
	r.inFlight[key] = call
	r.mu.Unlock()

	results, err := r.searchProviders(ctx, kind, input, limit)

	r.mu.Lock()
	call.results = append([]Result(nil), results...)
	call.err = err
	delete(r.inFlight, key)
	close(call.done)
	r.mu.Unlock()
	if err != nil {
		return results, err
	}
	return results, nil
}

// searchProviders runs the provider chain for a search and memoizes the result.
// The caller must already hold the in-flight claim for this key.
func (r *Resolver) searchProviders(ctx context.Context, kind Kind, input Input, limit int) ([]Result, error) {
	results := make([]Result, 0, len(r.providers))
	for _, provider := range r.providers {
		if provider == nil {
			continue
		}
		got, searched := r.searchProvider(provider, ctx, kind, input, limit)
		if !searched {
			result, err := provider.Lookup(ctx, kind, input)
			if err != nil || result == nil || result.URL == "" {
				continue
			}
			got = []Result{*result}
		}
		for _, result := range got {
			if result.URL == "" {
				continue
			}
			result.TrackName = firstNonEmpty(result.TrackName, input.TrackName)
			result.ArtistName = firstNonEmpty(result.ArtistName, input.ArtistName)
			result.AlbumName = firstNonEmpty(result.AlbumName, input.AlbumName)
			results = append(results, result)
		}
	}
	r.mu.Lock()
	r.search[cacheKey(kind, input)] = searchCacheEntry{results: append([]Result(nil), results...), expiresAt: time.Now().Add(positiveCacheTTL)}
	r.mu.Unlock()
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// searchProvider collects a provider's multi-candidate results. It returns
// searched=false when the provider has no SearchProvider for this kind, in
// which case the caller falls back to Lookup. SearchProviders that return
// ErrNotFound signal the kind is not supported by their multi-result search.
func (r *Resolver) searchProvider(provider Provider, ctx context.Context, kind Kind, input Input, limit int) ([]Result, bool) {
	sp, ok := provider.(SearchProvider)
	if !ok || sp == nil {
		return nil, false
	}
	got, err := sp.Search(ctx, kind, input, limit)
	if err == nil {
		return got, true
	}
	if errors.Is(err, ErrNotFound) {
		// Kind unsupported by multi-result search: fall back to Lookup.
		return nil, false
	}
	// Transient/provider error: skip this provider entirely.
	return nil, true
}

// Lookup walks the providers in order and returns the first non-empty URL. A
// miss is memoized as a negative cache entry.
func (r *Resolver) Lookup(ctx context.Context, kind Kind, input Input) (*Result, error) {
	for _, candidate := range candidateInputs(input) {
		if result, err := r.lookupOne(ctx, kind, candidate); err == nil {
			return result, nil
		}
	}
	return nil, ErrNotFound
}

func (r *Resolver) lookupOne(ctx context.Context, kind Kind, input Input) (*Result, error) {
	input = normalizeInput(input)
	key := cacheKey(kind, input)
	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && time.Now().Before(entry.expiresAt) {
		r.mu.Unlock()
		if entry.notFound {
			return nil, ErrNotFound
		}
		return entry.result, nil
	}
	// Join a lookup that is already running for this key rather than starting a
	// duplicate. Both waiters and the leader end up with the same memoized
	// answer, and the providers are asked once.
	if call, ok := r.lookupInFlight[key]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.result, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &lookupCall{done: make(chan struct{})}
	r.lookupInFlight[key] = call
	r.mu.Unlock()

	result, err := r.lookupProviders(ctx, kind, input, key)

	r.mu.Lock()
	call.result = result
	call.err = err
	delete(r.lookupInFlight, key)
	close(call.done)
	r.mu.Unlock()
	return result, err
}

// lookupProviders walks the chain for an exact lookup and memoizes the outcome.
// The caller must already hold the in-flight claim for this key.
func (r *Resolver) lookupProviders(ctx context.Context, kind Kind, input Input, key string) (*Result, error) {
	for _, provider := range r.providers {
		if provider == nil {
			continue
		}
		result, err := provider.Lookup(ctx, kind, input)
		if err == nil && result != nil && result.URL != "" && (kind != Song || songResultMatches(input, *result)) {
			r.store(key, result, false)
			return result, nil
		}
		if err == nil {
			err = ErrNotFound
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		// Transient/provider error without a result: keep trying the next tier
		// rather than recording a miss.
	}

	r.store(key, nil, true)
	return nil, ErrNotFound
}

func (r *Resolver) store(key string, result *Result, notFound bool) {
	ttl := negativeCacheTTL
	if !notFound {
		ttl = positiveCacheTTL
	}
	r.mu.Lock()
	r.cache[key] = cacheEntry{result: result, notFound: notFound, expiresAt: time.Now().Add(ttl)}
	r.mu.Unlock()
}
