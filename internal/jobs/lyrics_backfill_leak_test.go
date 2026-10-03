package jobs

import (
	"context"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

// TestLyricsRunDoesNotLeakTheWriteWatcher pins the goroutine budget of one lyrics
// run.
//
// The watcher that cancels the workers on a write abort is only ever woken by that
// abort, so a clean run left it blocked forever. Harmless for a one-shot CLI, wrong
// the moment this job is driven in a loop, and invisible without a count.
func TestLyricsRunDoesNotLeakTheWriteWatcher(t *testing.T) {
	before := runtime.NumGoroutine()

	const runs = 5
	for i := 0; i < runs; i++ {
		run := &lyricsBackfillRun{
			opts:           Options{DryRun: true},
			gate:           NewGate(0),
			progress:       NewProgress(io.Discard, nil),
			slots:          1,
			maxWriteErrors: 0,
			writeAbort:     make(chan struct{}),
			writeOK:        make(chan struct{}, 1),
			watchDone:      make(chan struct{}),
		}
		// No lanes and no database is the shape of a clean finish: there is no work,
		// so the writer never fails and writeAbort is never closed.
		if err := run.execute(context.Background(), 0); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	// Goroutine teardown is not instantaneous, so poll rather than sampling once.
	deadline := time.Now().Add(2 * time.Second)
	after := before
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= before+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutines grew from %d to %d across %d runs: about %d leaked per run",
		before, after, runs, (after-before)/runs)
}

// A song that needed no upstream request must be counted separately from one that
// was fetched. Conflating them would make a first run over an already-whole
// library look like it had done a great deal of upstream work.
//
// A fetched answer and a settled song are separate numbers too, because they are
// different units: one song produces several answers, and a run can write several
// answers without settling any song if it is stopped partway through the first.
func TestLyricsSummarySeparatesCachedFromFetched(t *testing.T) {
	run := &lyricsBackfillRun{mu: sync.Mutex{}}
	run.countCommitted([]lyricsWriteOp{
		{answer: &lyricsHit{provider: "lrclib", lyrics: &db.Lyrics{PlainLyrics: "a"}}},
		{answer: &lyricsHit{provider: "kugou", lyrics: &db.Lyrics{PlainLyrics: "b"}}},
		{answer: &lyricsHit{provider: "paxsenix", lyrics: &db.Lyrics{SyncedLyrics: "b"}}},
		{settle: true},
		{settleUnknownAge: true},
	})
	run.noteMissed()
	run.noteResumed()
	summary := run.summary()
	for _, want := range []string{
		"1 tracks settled",
		"3 lyrics fetched",
		"1 not found upstream",
		"1 already cached locally",
		"1 resumed from a partial run",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q is missing %q", summary, want)
		}
	}
}

// Zero-valued counters must not be reported: an operator should not have to read
// past a list of "0 rate limited" clauses to find the number they care about.
func TestLyricsSummaryOmitsZeroCounters(t *testing.T) {
	run := &lyricsBackfillRun{mu: sync.Mutex{}}
	summary := run.summary()
	for _, unwanted := range []string{
		"already cached", "resumed", "rate limited", "failed", "rejected", "write errors",
	} {
		if strings.Contains(summary, unwanted) {
			t.Errorf("summary %q should not mention %q when it is zero", summary, unwanted)
		}
	}
	for _, want := range []string{"0 tracks settled", "0 lyrics fetched", "0 not found upstream"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q should still report %q", summary, want)
		}
	}
}
