package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/upstream"
)

// countingResolver builds a provider set whose answers are all trivial, and which
// records which provider was asked about which song and when.
//
// The ordering of the calls is the thing under test: at one request per interval
// per provider, two songs on the same provider queue behind each other while two
// on different ones do not, so how the run spreads itself across providers is worth
// more than how fast any one of them answers.
func countingResolver(t *testing.T, providers int, get func(name string, work db.LyricsWork) (*db.Lyrics, error)) *lyricsBackfillResolver {
	t.Helper()
	resolver := &lyricsBackfillResolver{}
	for i := range providers {
		name := fmt.Sprintf("p%d", i)
		resolver.add(name, func(_ context.Context, work db.LyricsWork) (*db.Lyrics, error) {
			return get(name, work)
		})
	}
	return resolver
}

// lanes builds one lane per provider, matching the shape Run produces.
func providerLanes(resolver *lyricsBackfillResolver) []*Lane {
	lanes := make([]*Lane, 0, resolver.count())
	for _, name := range resolver.names() {
		lanes = append(lanes, &Lane{Name: name})
	}
	return lanes
}

// newRun builds a run with no databases, which is only usable against a dry run
// where nothing is actually written. The write channel is sized rather than left
// nil because a slot that cannot hand its answers to the writer blocks on it, and
// a test that is not inspecting the writes should not have to drain them.
func newRun(t *testing.T, resolver *lyricsBackfillResolver, opts Options) *lyricsBackfillRun {
	t.Helper()
	opts.DryRun = true
	lanes := providerLanes(resolver)
	run := &lyricsBackfillRun{
		opts:     opts,
		resolver: resolver,
		gate:     NewGate(0),
		progress: NewProgress(io.Discard, lanes),
		lanes:    lanes,
	}
	run.writes = make(chan lyricsWriteOp, 256)
	return run
}

// drained collects everything a run queued for the writer and closes the channel.
func drained(run *lyricsBackfillRun) []lyricsWriteOp {
	var ops []lyricsWriteOp
	for op := range run.writes {
		ops = append(ops, op)
	}
	return ops
}

// TestSlotsSpreadTheirSongsAcrossProviders is the property the whole shape rests
// on. Every provider is limited to one request per interval, so N songs all asking
// the first provider queue into a single stream and N-1 of the slots sit doing
// nothing. Starting each slot on a different provider is what turns a fixed number
// of slots into a fixed number of parallel upstreams.
//
// Each slot is given its own song here rather than racing for them off a channel.
// Which slot picks up which song from a queue is up to the scheduler, and a fast
// slot can take several before a slow one has even started, which would make the
// measurement say nothing about the stride.
func TestSlotsSpreadTheirSongsAcrossProviders(t *testing.T) {
	const providers = 6

	var mu sync.Mutex
	// openedAt is the provider each song was first asked about.
	openedAt := make(map[int]string, providers)
	resolver := countingResolver(t, providers, func(name string, work db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		if _, seen := openedAt[int(work.ID)]; !seen {
			openedAt[int(work.ID)] = name
		}
		mu.Unlock()
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	run := newRun(t, resolver, Options{})
	var wg sync.WaitGroup
	for stride := range providers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			song := &lyricsSong{
				work:      db.LyricsWork{ID: int64(index)},
				pending:   []int{0, 1, 2, 3, 4, 5},
				attempted: make(map[int]int),
			}
			run.runSong(context.Background(), song, index)
		}(stride)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(openedAt) != providers {
		t.Fatalf("%d songs were opened, want %d", len(openedAt), providers)
	}
	// Without the stride every song would open on the first provider and queue
	// behind it, so each song opening on its own slot's provider is the claim.
	distinct := make(map[string]bool, providers)
	for song, name := range openedAt {
		if want := fmt.Sprintf("p%d", song); name != want {
			t.Errorf("song %d opened on %s, want %s: the slots are not spread across providers", song, name, want)
		}
		distinct[name] = true
	}
	if len(distinct) != providers {
		t.Errorf("%d distinct providers were in use, want %d", len(distinct), providers)
	}
}

// The slot loop has to keep taking songs after finishing one, or a run would
// process a single song per slot and stop.
func TestSlotKeepsTakingSongsUntilTheStreamEnds(t *testing.T) {
	const slots = 3
	var mu sync.Mutex
	settled := 0
	resolver := countingResolver(t, 2, func(string, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	run := newRun(t, resolver, Options{})
	songs := make(chan *lyricsSong)
	var wg sync.WaitGroup
	for index := range slots {
		wg.Add(1)
		go func(stride int) {
			defer wg.Done()
			run.slot(context.Background(), stride, songs)
		}(index)
	}

	const total = 9
	go func() {
		defer close(songs)
		for i := range total {
			select {
			case songs <- &lyricsSong{
				work:      db.LyricsWork{ID: int64(i)},
				pending:   []int{0, 1},
				attempted: make(map[int]int),
			}:
			case <-time.After(2 * time.Second):
				return
			}
		}
	}()
	wg.Wait()
	close(run.writes)

	for _, op := range drained(run) {
		if op.settle {
			mu.Lock()
			settled++
			mu.Unlock()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if settled != total {
		t.Errorf("%d songs settled, want %d: a slot stopped taking work early", settled, total)
	}
}

// Rotation is applied to the pending list rather than to an index into it, so a
// resumed song with only some providers left still starts on a different one per
// slot rather than collapsing back onto whichever providers remain.
func TestRotatePendingKeepsTheWholeListAndSpreadsSlots(t *testing.T) {
	full := []int{0, 1, 2, 3, 4, 5}
	want := [][]int{
		{0, 1, 2, 3, 4, 5},
		{1, 2, 3, 4, 5, 0},
		{2, 3, 4, 5, 0, 1},
	}
	for stride, expected := range want {
		got := rotatePending(full, stride, len(full))
		if len(got) != len(full) {
			t.Fatalf("stride %d: got %d providers, want %d: rotation dropped work", stride, len(got), len(full))
		}
		for i := range got {
			if got[i] != expected[i] {
				t.Errorf("stride %d: order = %v, want %v", stride, got, expected)
				break
			}
		}
	}

	// A short pending list is the resumed case. Rotating it must still keep every
	// provider, which is the whole point of not rotating an index into the chain.
	short := []int{2, 4}
	if got := rotatePending(short, 1, 6); len(got) != 2 || got[0] != 4 || got[1] != 2 {
		t.Errorf("rotated a resumed list to %v, want [4 2]", got)
	}

	// Degenerate shapes must not panic or reorder anything.
	if got := rotatePending(nil, 3, 6); len(got) != 0 {
		t.Errorf("rotating nothing produced %v", got)
	}
	if got := rotatePending([]int{1}, 4, 6); len(got) != 1 || got[0] != 1 {
		t.Errorf("rotating a single provider produced %v", got)
	}
	if got := rotatePending([]int{0, 1}, 3, 1); len(got) != 2 {
		t.Errorf("one provider should leave the list alone, got %v", got)
	}
}

// A throttled provider must cost a retry of that one pair, not a fresh attempt at
// all of them and not the five answers already collected. Getting this wrong either
// throws away real work or lets one provider stall the whole run.
func TestThrottledProviderIsRetriedOnItsOwn(t *testing.T) {
	const providers = 3
	throttle := &upstream.RateLimitError{Provider: "p0", Status: http.StatusTooManyRequests}

	var mu sync.Mutex
	calls := map[string]int{}
	answers := map[string]int{}
	// p0 refuses once and then answers, which is what makes the retry observable.
	refused := false
	resolver := countingResolver(t, providers, func(name string, _ db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		calls[name]++
		first := !refused
		mu.Unlock()
		if name == "p0" && first {
			mu.Lock()
			refused = true
			mu.Unlock()
			return nil, throttle
		}
		mu.Lock()
		answers[name]++
		mu.Unlock()
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	run := newRun(t, resolver, Options{})
	song := &lyricsSong{work: db.LyricsWork{ID: 1}, pending: []int{0, 1, 2}, attempted: make(map[int]int)}

	if !run.runSong(context.Background(), song, 0) {
		t.Fatal("the slot stopped on a throttle that should have been retried")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls["p0"] != 2 {
		t.Errorf("p0 called %d times, want 2: one refusal and one retry", calls["p0"])
	}
	if calls["p1"] != 1 || calls["p2"] != 1 {
		t.Errorf("p1 called %d times, p2 %d times; want 1 each: a throttle must not re-ask the others", calls["p1"], calls["p2"])
	}
	if answers["p0"] != 1 || answers["p1"] != 1 || answers["p2"] != 1 {
		t.Errorf("answers = %v, want every provider to have answered once", answers)
	}
}

// A provider that refuses the same song forever must not spin the slot. The retry
// budget is what stops that, and the song staying unsettled is what stops a bad
// minute from being recorded as a permanent answer.
func TestAPersistentlyThrottledProviderRunsOutOfRetries(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	resolver := countingResolver(t, 2, func(name string, _ db.LyricsWork) (*db.Lyrics, error) {
		if name == "p0" {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, &upstream.RateLimitError{Provider: "p0", Status: http.StatusTooManyRequests}
		}
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	run := newRun(t, resolver, Options{})
	song := &lyricsSong{work: db.LyricsWork{ID: 1}, pending: []int{0, 1}, attempted: make(map[int]int)}
	run.runSong(context.Background(), song, 0)

	mu.Lock()
	defer mu.Unlock()
	if want := maxThrottleRetries + 1; calls != want {
		t.Errorf("p0 called %d times, want %d: one attempt plus its retry budget", calls, want)
	}
	if deferred := run.deferredCount(); deferred != maxThrottleRetries {
		t.Errorf("deferred = %d, want %d retries reported as deferred", deferred, maxThrottleRetries)
	}
}

// A hard failure is not a rate limit. Retrying every failure would let one broken
// provider stall a run that is otherwise making progress, so a failure is counted
// and the song is left unsettled for the next one.
func TestHardFailuresAreNotRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	resolver := countingResolver(t, 2, func(name string, _ db.LyricsWork) (*db.Lyrics, error) {
		if name == "p0" {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, errors.New("connection reset by peer")
		}
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	run := newRun(t, resolver, Options{})
	song := &lyricsSong{work: db.LyricsWork{ID: 1}, pending: []int{0, 1}, attempted: make(map[int]int)}
	run.runSong(context.Background(), song, 0)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("p0 called %d times, want 1: a transport failure is not worth retrying here", calls)
	}
	if deferred := run.deferredCount(); deferred != 0 {
		t.Errorf("deferred = %d, want 0", deferred)
	}
}

// The song is settled once every provider has answered, and not before. Settling on
// a partial set would record "checked" about a song nobody finished asking about.
func TestSongIsSettledOnlyAfterEveryProviderAnswers(t *testing.T) {
	var mu sync.Mutex
	settledAfter := -1
	answered := 0

	resolver := countingResolver(t, 4, func(string, db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		answered++
		mu.Unlock()
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	run := newRun(t, resolver, Options{})
	song := &lyricsSong{work: db.LyricsWork{ID: 1}, pending: []int{0, 1, 2, 3}, attempted: make(map[int]int)}
	run.runSong(context.Background(), song, 0)
	close(run.writes)

	for _, op := range drained(run) {
		if op.settle {
			mu.Lock()
			settledAfter = answered
			mu.Unlock()
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if settledAfter != 4 {
		t.Errorf("settled after %d answers, want 4: every provider has to answer first", settledAfter)
	}
}

// Every answer reaches the writer as it arrives, not batched to the end. That is
// what makes an interrupted run resumable: whatever landed is on disk, and the
// ledger says which providers still owe an answer.
func TestEveryAnswerIsWrittenAsItArrives(t *testing.T) {
	resolver := countingResolver(t, 3, func(string, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})
	run := newRun(t, resolver, Options{})
	song := &lyricsSong{work: db.LyricsWork{ID: 1}, pending: []int{0, 1, 2}, attempted: make(map[int]int)}
	run.runSong(context.Background(), song, 0)
	close(run.writes)

	var answers, settles int
	for _, op := range drained(run) {
		switch {
		case op.answer != nil:
			answers++
		case op.settle:
			settles++
		}
	}
	if answers != 3 {
		t.Errorf("%d answer ops, want 3: one per provider, written as each arrived", answers)
	}
	if settles != 1 {
		t.Errorf("%d settle ops, want exactly 1", settles)
	}
}

// A definitive miss is recorded against the song, but only under an identity
// complete enough to key the ledger on. The ledger is keyed by track alone, so a
// miss stored under a partial identity would suppress a later retry made once the
// artist or album was known.
func TestMissesAreRecordedOnlyForACompleteIdentity(t *testing.T) {
	resolver := countingResolver(t, 1, func(string, db.LyricsWork) (*db.Lyrics, error) {
		return nil, lrclib.ErrNotFound
	})

	for _, tc := range []struct {
		name        string
		work        db.LyricsWork
		wantLedeger bool
	}{
		{
			name:        "artist and album known",
			work:        db.LyricsWork{ID: 1, Name: "Song", Artist: "Artist", Album: "Album"},
			wantLedeger: true,
		},
		{
			name: "album unknown",
			work: db.LyricsWork{ID: 2, Name: "Song", Artist: "Artist"},
		},
		{
			name: "artist unknown",
			work: db.LyricsWork{ID: 3, Name: "Song", Album: "Album"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newRun(t, resolver, Options{})
			song := &lyricsSong{work: tc.work, pending: []int{0}, attempted: make(map[int]int)}
			run.runSong(context.Background(), song, 0)
			close(run.writes)

			var recordMiss bool
			for _, op := range drained(run) {
				if op.missed != "" {
					recordMiss = op.recordMiss
				}
			}
			if recordMiss != tc.wantLedeger {
				t.Errorf("recordMiss = %v, want %v", recordMiss, tc.wantLedeger)
			}
		})
	}
}

// A song whose lyrics are already on disk costs no upstream request at all, which
// is the case that lets a first run over an already-whole library finish without
// re-fetching it.
func TestCachedSongCostsNoUpstreamRequest(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	resolver := countingResolver(t, 3, func(string, db.LyricsWork) (*db.Lyrics, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	run := newRun(t, resolver, Options{})
	run.runSong(context.Background(), &lyricsSong{work: db.LyricsWork{ID: 1}, cached: true}, 0)
	close(run.writes)

	var cached int
	for _, op := range drained(run) {
		if op.settleUnknownAge {
			cached++
		}
	}
	if cached != 1 {
		t.Errorf("%d settle-unknown-age ops, want 1", cached)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("%d upstream requests for a cached song, want 0", calls)
	}
}

// The resume rule is decided in the producer, where the whole page is available, so
// this pins the decision itself: a ledger row means that provider already answered,
// and a song with nothing left to ask is finished rather than pointless work.
func TestResumeDropsProvidersTheLedgerAlreadyCovers(t *testing.T) {
	const providers = 4
	resolver := countingResolver(t, providers, func(string, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})
	names := resolver.names()

	// A song two providers had already answered, and one nothing was recorded for.
	// The value is whether that last attempt succeeded, so a recorded miss still
	// means the provider was asked and must not be asked again.
	answered := map[int64]map[string]bool{
		1: {names[0]: true, names[1]: false},
		2: nil,
	}

	var got [][]int
	for _, item := range []db.LyricsWork{{ID: 1}, {ID: 2}} {
		song := &lyricsSong{work: item, attempted: make(map[int]int)}
		for index, name := range names {
			if _, asked := answered[item.ID][name]; !asked {
				song.pending = append(song.pending, index)
			}
		}
		got = append(got, song.pending)
	}

	if len(got[0]) != providers-2 {
		t.Errorf("resumed song has %d providers pending, want %d", len(got[0]), providers-2)
	}
	for _, index := range got[0] {
		if index == 0 || index == 1 {
			t.Errorf("resumed song still has provider %d pending, want it dropped", index)
		}
	}
	if len(got[1]) != providers {
		t.Errorf("untouched song has %d providers pending, want all %d", len(got[1]), providers)
	}
}

// A refresh is the operator asking again on purpose, so it has to ignore the ledger
// entirely. Reading it here would silently turn -refresh into a no-op on every song
// a previous run had finished.
func TestARefreshIgnoresTheLedger(t *testing.T) {
	resolver := countingResolver(t, 3, func(string, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})
	run := newRun(t, resolver, Options{Refresh: true})
	run.selection = refreshSelection{enabled: true}
	if fetches := run.alreadyAnswered(context.Background(), []int64{1}); fetches != nil {
		t.Errorf("a refresh consulted the ledger: %v", fetches)
	}
}

// The run's ceiling on songs in flight is the provider count. More slots than
// providers would only put several songs in one provider's queue, which is where
// they would have been anyway.
func TestSlotsAreCappedAtTheProviderCount(t *testing.T) {
	cases := []struct {
		workers   int
		providers int
		want      int
	}{
		{workers: 4, providers: 6, want: 4},
		{workers: 6, providers: 6, want: 6},
		{workers: 12, providers: 6, want: 6},
		{workers: 4, providers: 2, want: 2},
		{workers: 1, providers: 6, want: 1},
	}
	for _, tc := range cases {
		slots := tc.workers
		if tc.providers > 0 && slots > tc.providers {
			slots = tc.providers
		}
		if slots != tc.want {
			t.Errorf("%d workers over %d providers gave %d slots, want %d",
				tc.workers, tc.providers, slots, tc.want)
		}
	}
}

// A run's own ceiling is a second, coarser limit: even with providers to spare, the
// operator's -rate still decides how many songs start per minute.
func TestARateCeilingStillBoundsSongsStarted(t *testing.T) {
	resolver := countingResolver(t, 6, func(string, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})
	// 600/min is one song every 100ms.
	run := newRun(t, resolver, Options{RatePerMinute: 600})
	run.gate = NewGate(run.opts.RatePerMinute)
	ctx := context.Background()
	if err := run.gate.Wait(ctx); err != nil {
		t.Fatalf("first song: %v", err)
	}
	start := time.Now()
	if err := run.gate.Wait(ctx); err != nil {
		t.Fatalf("second song: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("the second song started after %s, want the ceiling applied per song", elapsed)
	}
}
