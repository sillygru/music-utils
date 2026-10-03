package jobs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/lrclib"
	"github.com/sillygru/music-utils/internal/upstream"
)

// A single provider's answer is classified on its own, and the song is only ever
// settled once all of them have answered. So the question each lookup has to answer
// is narrow: did this one provider give a definitive answer, and if not, was that
// because it has nothing or because we could not find out?
//
// Everything except a definitive negative and a usable answer must stay unsettled,
// which is the property that stops a bad minute from becoming a permanent
// statement about a song.
func TestLookupClassifiesEachAnswerOnItsOwn(t *testing.T) {
	cases := []struct {
		name     string
		provider func(context.Context, db.LyricsWork) (*db.Lyrics, error)
		want     Outcome
	}{
		{
			name: "found lyrics",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return &db.Lyrics{PlainLyrics: "la"}, nil
			},
			want: OutcomeSucceeded,
		},
		{
			name: "found instrumental",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return &db.Lyrics{Instrumental: true}, nil
			},
			want: OutcomeSucceeded,
		},
		{
			name: "found synced",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return &db.Lyrics{SyncedLyrics: "[00:01.00]timed"}, nil
			},
			want: OutcomeSucceeded,
		},
		{
			name: "the provider's own not-found sentinel",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return nil, lrclib.ErrNotFound
			},
			want: OutcomeMissing,
		},
		{
			// A provider that answers with nothing usable has told us it has no
			// lyrics, which is a different thing from one that failed.
			name: "an empty answer",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return &db.Lyrics{}, nil
			},
			want: OutcomeMissing,
		},
		{
			name: "a nil answer with no error",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return nil, nil
			},
			want: OutcomeMissing,
		},
		{
			// A refusal is about the caller, not the song, so nothing was
			// established about whether lyrics exist.
			name: "a provider refused this host",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return nil, &upstream.RejectedError{Provider: "lrclib", Status: http.StatusForbidden}
			},
			want: OutcomeFailed,
		},
		{
			name: "a provider errored",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return nil, errors.New("dial tcp: refused")
			},
			want: OutcomeFailed,
		},
		{
			name: "a provider throttled",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return nil, &upstream.RateLimitError{Provider: "lrclib", Status: http.StatusTooManyRequests}
			},
			want: OutcomeThrottled,
		},
		{
			// A throttle that advertises its cooldown is the one case worth
			// retrying inside this run, so it must not be confused with a plain
			// failure.
			name: "a throttle that advertises a cooldown",
			provider: func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
				return nil, &upstream.RateLimitError{
					Provider:   "lrclib",
					Status:     http.StatusTooManyRequests,
					RetryAfter: 5 * time.Second,
				}
			},
			want: OutcomeThrottled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &lyricsBackfillResolver{}
			resolver.add("provider", tc.provider)

			got := resolver.Lookup(context.Background(), 0, db.LyricsWork{Name: "Song"})
			if got.outcome != tc.want {
				t.Errorf("outcome = %v, want %v", got.outcome, tc.want)
			}
		})
	}
}

// The property the whole design rests on: none of these transient failures may be
// recorded as a definitive negative, because that is what would let a song be
// recorded as having no lyrics because of a bad minute.
func TestLookupNeverReportsATransientFailureAsAMiss(t *testing.T) {
	transient := []func(context.Context, db.LyricsWork) (*db.Lyrics, error){
		func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
			return nil, errors.New("connection refused")
		},
		func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
			return nil, &upstream.RejectedError{Provider: "lrclib", Status: http.StatusForbidden}
		},
		func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
			return nil, &upstream.RateLimitError{Provider: "lrclib", Status: http.StatusTooManyRequests}
		},
		// A wrapped transport failure is still a transport failure.
		func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
			return nil, fmt.Errorf("provider: %w", errors.New("dial tcp: connection refused"))
		},
	}
	for i, provider := range transient {
		resolver := &lyricsBackfillResolver{}
		resolver.add("provider", provider)

		got := resolver.Lookup(context.Background(), 0, db.LyricsWork{Name: "Song"})
		if got.outcome == OutcomeMissing {
			t.Errorf("case %d: a transient failure was reported as a definitive miss", i)
		}
	}
}

// The advertised cooldown has to survive to the caller: it is what the retry
// budget and the run's global pause are both built on.
func TestLookupCarriesTheAdvertisedCooldown(t *testing.T) {
	resolver := &lyricsBackfillResolver{}
	resolver.add("throttled", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		return nil, &upstream.RateLimitError{
			Provider:   "throttled",
			Status:     http.StatusTooManyRequests,
			RetryAfter: 45 * time.Second,
		}
	})

	got := resolver.Lookup(context.Background(), 0, db.LyricsWork{Name: "Song"})
	if got.retryAfter != 45*time.Second {
		t.Errorf("retryAfter = %s, want 45s", got.retryAfter)
	}
}

// A refusal has to name itself, because the run pauses for it and the operator
// needs to know which upstream stopped talking to them.
func TestLookupCarriesTheRejectionSummary(t *testing.T) {
	resolver := &lyricsBackfillResolver{}
	resolver.add("refused", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		return nil, &upstream.RejectedError{Provider: "refused", Status: http.StatusForbidden}
	})

	got := resolver.Lookup(context.Background(), 0, db.LyricsWork{Name: "Song"})
	if !got.rejected {
		t.Fatal("a refusal was not surfaced")
	}
	if got.rejection == "" || got.rejection == "no provider detail" {
		t.Errorf("rejection = %q, want it to name the provider", got.rejection)
	}
}

// Asking one provider must not consult any other. The whole point of the walk is
// that the run controls the order and the spacing, and a lookup that reached round
// to a neighbour would take that away.
func TestLookupTouchesOnlyTheProviderItWasGiven(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	resolver := &lyricsBackfillResolver{}
	add := func(name string, get func(context.Context, db.LyricsWork) (*db.Lyrics, error)) {
		resolver.add(name, func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error) {
			mu.Lock()
			calls[name]++
			mu.Unlock()
			return get(ctx, work)
		})
	}
	add("first", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})
	add("second", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		t.Error("the second provider was consulted during a lookup of the first")
		return nil, lrclib.ErrNotFound
	})

	if got := resolver.Lookup(context.Background(), 0, db.LyricsWork{Name: "Song"}); got.outcome != OutcomeSucceeded {
		t.Fatalf("outcome = %v, want OutcomeSucceeded", got.outcome)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls["first"] != 1 {
		t.Errorf("the provider was asked %d times, want 1", calls["first"])
	}
	if calls["second"] != 0 {
		t.Errorf("the second provider was asked %d times, want 0", calls["second"])
	}
}

// An index outside the configured set is a programming error, and it must not be
// mistaken for a miss: treating it as one would settle a song nobody asked about.
func TestLookupRejectsAnUnconfiguredProvider(t *testing.T) {
	resolver := &lyricsBackfillResolver{}
	resolver.add("only", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})

	for _, index := range []int{-1, 1, 99} {
		got := resolver.Lookup(context.Background(), index, db.LyricsWork{Name: "Song"})
		if got.outcome == OutcomeMissing {
			t.Errorf("index %d: an unconfigured provider was reported as a miss", index)
		}
		if got.err == nil {
			t.Errorf("index %d: an unconfigured provider produced no error", index)
		}
	}
}

// An interrupted run must spend nothing upstream, so the check happens before the
// provider is called rather than after it returns.
func TestLookupStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resolver := &lyricsBackfillResolver{}
	resolver.add("anything", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		t.Error("a cancelled context still reached the provider")
		return &db.Lyrics{PlainLyrics: "should never be reached"}, nil
	})

	got := resolver.Lookup(ctx, 0, db.LyricsWork{Name: "Song"})
	if got.lyrics != nil {
		t.Error("a cancelled context produced an answer")
	}
	if got.outcome == OutcomeMissing {
		t.Error("a cancelled lookup must not be a confirmed miss")
	}
}

// Every provider is asked about every song, so the run's promise depends on a
// walk that reaches the end of the chain rather than stopping early. The counter is
// the only thing that proves it: a walk that gave up after the first usable answer
// would leave the later providers at zero.
func TestEveryProviderIsAskedAboutEverySong(t *testing.T) {
	const providers = 6

	var mu sync.Mutex
	calls := map[string]int{}
	resolver := &lyricsBackfillResolver{}
	add := func(name string, get func(context.Context, db.LyricsWork) (*db.Lyrics, error)) {
		resolver.add(name, func(ctx context.Context, work db.LyricsWork) (*db.Lyrics, error) {
			mu.Lock()
			calls[name]++
			mu.Unlock()
			return get(ctx, work)
		})
	}
	add("first", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		return nil, lrclib.ErrNotFound
	})
	add("second", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		return nil, lrclib.ErrNotFound
	})
	// A hit in the middle must not end the walk: every other answer is worth
	// having on disk, and the song cannot be settled until all of them are in.
	add("third", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{PlainLyrics: "words"}, nil
	})
	add("fourth", func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
		return &db.Lyrics{SyncedLyrics: "[00:01.00]timed"}, nil
	})
	for i := range providers - 4 {
		add(fmt.Sprintf("extra-%d", i), func(context.Context, db.LyricsWork) (*db.Lyrics, error) {
			return &db.Lyrics{PlainLyrics: fmt.Sprintf("words %d", i)}, nil
		})
	}

	work := db.LyricsWork{Name: "Song"}
	var hits []lyricsHit
	for i := range resolver.count() {
		if got := resolver.Lookup(context.Background(), i, work); got.outcome == OutcomeSucceeded {
			hits = append(hits, lyricsHit{provider: resolver.names()[i], lyrics: got.lyrics})
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != providers {
		t.Fatalf("%d providers were asked, want %d", len(calls), providers)
	}
	for name, count := range calls {
		if count != 1 {
			t.Errorf("provider %q asked %d times, want 1", name, count)
		}
	}
	if len(hits) != providers-2 {
		t.Errorf("hits = %d, want every answer but the two misses", len(hits))
	}
}

// The job stores exactly what the live path would have served, so a result the
// server treats as unavailable must not be written.
func TestUsableLyricsMatchesTheLiveRequestPath(t *testing.T) {
	usable := []*db.Lyrics{
		{PlainLyrics: "words"},
		{SyncedLyrics: "[00:01.00]la"},
		{Instrumental: true},
	}
	for _, lyrics := range usable {
		if !usableLyrics(lyrics) {
			t.Errorf("%+v should be usable", lyrics)
		}
	}
	unusable := []*db.Lyrics{
		nil,
		{},
		{PlainLyrics: ""},
		{SyncedLyrics: ""},
	}
	for _, lyrics := range unusable {
		if usableLyrics(lyrics) {
			t.Errorf("%+v should not be usable", lyrics)
		}
	}
}

// A lookup that established nothing must not suppress the live path's future
// lookups, which is exactly what recording a miss would do.
func TestIsNotFoundRecognizesEveryProviderSentinel(t *testing.T) {
	for _, miss := range providerMisses {
		if !isNotFound(miss) {
			t.Errorf("%v should be recognized as a definitive miss", miss)
		}
		if !isNotFound(fmt.Errorf("provider: %w", miss)) {
			t.Errorf("%v should be recognized through wrapping", miss)
		}
	}
	if isNotFound(errors.New("connection refused")) {
		t.Error("a transport failure is not a definitive miss")
	}
	if isNotFound(nil) {
		t.Error("nil is not a definitive miss")
	}
}
