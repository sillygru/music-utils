package cover

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowProvider blocks until released, so a burst of identical callers is
// guaranteed to overlap. Without coalescing each of them runs its own provider
// call, which is exactly the shape that queues behind the shared upstream
// pacers.
//
// It honors context cancellation while blocked. A stub that ignored ctx would
// hang instead of failing if coalescing regressed, which turns a broken build
// into a stuck test run.
type slowProvider struct {
	mu      sync.Mutex
	release chan struct{}
	calls   atomic.Int64
	result  *Result
}

func newSlowProvider(result *Result) *slowProvider {
	return &slowProvider{release: make(chan struct{}), result: result}
}

func (p *slowProvider) unblock() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.release:
	default:
		close(p.release)
	}
}

func (p *slowProvider) Name() string { return "slow" }

func (p *slowProvider) Lookup(ctx context.Context, _ Kind, _ Input) (*Result, error) {
	p.calls.Add(1)
	select {
	case <-p.wait():
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.result == nil {
		return nil, ErrNotFound
	}
	return p.result, nil
}

func (p *slowProvider) wait() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.release
}

// Concurrent identical lookups share one provider fan-out. The providers are
// paced, so N duplicate calls is not merely N times the work: it is N times the
// wait, and the last caller waits for all the others.
func TestResolverCoalescesConcurrentLookups(t *testing.T) {
	provider := newSlowProvider(&Result{URL: "http://img/cover.jpg", Source: "slow", ArtistName: "Artist"})
	resolver := NewResolver(provider)

	const callers = 10
	results := make([]*Result, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = resolver.Lookup(context.Background(), Artist, Input{ArtistName: "Artist"})
		}(i)
	}

	// Give every goroutine time to reach the resolver before the provider is
	// released; a caller that arrived late would otherwise be served from the
	// memo and hide a missing coalesce.
	time.Sleep(50 * time.Millisecond)
	provider.unblock()
	wg.Wait()

	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider was called %d times for one coalesced lookup, want 1", calls)
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] == nil || results[i].URL != "http://img/cover.jpg" {
			t.Fatalf("caller %d got %+v, want the resolved cover", i, results[i])
		}
	}
}

// Concurrent identical searches share one fan-out too, for the same reason.
func TestResolverCoalescesConcurrentSearches(t *testing.T) {
	provider := newSlowProvider(&Result{URL: "http://img/album.jpg", Source: "slow", AlbumName: "Album"})
	resolver := NewResolver(provider)

	const callers = 10
	counts := make([]int, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var results []Result
			results, errs[i] = resolver.Search(context.Background(), Album, Input{AlbumName: "Album"}, 10)
			counts[i] = len(results)
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	provider.unblock()
	wg.Wait()

	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider was called %d times for one coalesced search, want 1", calls)
	}
	for i := range counts {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if counts[i] != 1 {
			t.Fatalf("caller %d got %d results, want 1", i, counts[i])
		}
	}
}

// A coalesced miss is memoized too, so the burst after it is served from cache.
func TestResolverCoalescesConcurrentMisses(t *testing.T) {
	provider := newSlowProvider(nil)
	resolver := NewResolver(provider)

	const callers = 6
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := resolver.Lookup(context.Background(), Artist, Input{ArtistName: "Nobody"}); err == nil {
				t.Error("expected a miss")
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	provider.unblock()
	wg.Wait()

	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider was called %d times for one coalesced miss, want 1", calls)
	}
	// The negative entry outlives the burst, so a later request costs nothing.
	if _, err := resolver.Lookup(context.Background(), Artist, Input{ArtistName: "Nobody"}); err == nil {
		t.Fatal("expected the memoized miss to be returned")
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("memoized miss still called the provider: %d calls", calls)
	}
}

// A caller that gives up while waiting does not strand the shared fan-out or the
// leader's result.
func TestResolverLookupWaiterHonoursContext(t *testing.T) {
	provider := newSlowProvider(&Result{URL: "http://img/cover.jpg", Source: "slow", ArtistName: "Artist"})
	resolver := NewResolver(provider)

	// Start a leader that will block on the provider.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		if _, err := resolver.Lookup(context.Background(), Artist, Input{ArtistName: "Artist"}); err != nil {
			t.Errorf("leader: %v", err)
		}
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := resolver.Lookup(ctx, Artist, Input{ArtistName: "Artist"}); err == nil {
		t.Fatal("expected the waiter's context to end its wait")
	}

	provider.unblock()
	<-leaderDone

	// The fan-out completed normally and memoized its answer.
	if _, err := resolver.Lookup(context.Background(), Artist, Input{ArtistName: "Artist"}); err != nil {
		t.Fatalf("after the leader finished: %v", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider was called %d times, want 1", calls)
	}
}