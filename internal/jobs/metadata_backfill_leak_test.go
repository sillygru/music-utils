package jobs

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"
)

// TestRunDoesNotLeakTheWriteWatcher pins the goroutine budget of one run.
//
// The watcher that cancels the workers on a write abort is only ever woken by
// that abort, so a clean run left it blocked forever. Harmless for a one-shot
// CLI, wrong the moment this job is driven in a loop, and invisible without a
// count.
func TestRunDoesNotLeakTheWriteWatcher(t *testing.T) {
	before := runtime.NumGoroutine()

	const runs = 5
	for i := 0; i < runs; i++ {
		run := &backfillRun{
			opts:           Options{DryRun: true},
			gate:           NewGate(0),
			progress:       NewProgress(io.Discard, nil),
			maxWriteErrors: 0,
			writeAbort:     make(chan struct{}),
			writeOK:        make(chan struct{}, 1),
			watchDone:      make(chan struct{}),
		}
		// No lanes and no database is the shape of a clean finish: there is no
		// work, so the writer never fails and writeAbort is never closed.
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
