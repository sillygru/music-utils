package pacer

import (
	"context"
	"testing"
	"time"
)

// Each class is paced at its own rate because the two callers are doing different
// jobs: someone is waiting on a live request, while a batch run is working through
// a library and nobody is waiting on it. Sharing one lease must not force them onto
// one number, or the faster side is throttled to the slower side's rate.
func TestEachClassIsSpacedAtItsOwnInterval(t *testing.T) {
	database := sharedTestDB(t)
	const (
		userInterval = 100 * time.Millisecond
		jobInterval  = 600 * time.Millisecond
	)
	shared := NewShared(database, "lrclib", userInterval, jobInterval, time.Millisecond)
	user := shared.ForUser()
	job := shared.ForJob()

	ctx := context.Background()
	// Prime both leases. The first claim on each is free.
	if err := user.Wait(ctx); err != nil {
		t.Fatalf("user wait: %v", err)
	}
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}

	// Three user claims in a row should cost two user intervals, and nowhere near
	// the job interval the lease also carries.
	start := time.Now()
	for range 3 {
		if err := user.Wait(ctx); err != nil {
			t.Fatalf("user wait: %v", err)
		}
	}
	elapsed := time.Since(start)
	// The lease stores admission times in whole milliseconds, so a wait computed
	// from it can land a fraction short of the interval. The bound below is loose
	// enough for that and still far tighter than the job interval the claim must
	// not be held to.
	if floor := 2*userInterval - 20*time.Millisecond; elapsed < floor {
		t.Errorf("three live claims took %s, want at least %s: live traffic is not spaced", elapsed, floor)
	}
	if elapsed > jobInterval {
		t.Errorf("three live claims took %s, want under the job interval %s: live traffic was held to the batch rate", elapsed, jobInterval)
	}
}

// The job rate is the one that has to be honoured exactly, because it is the whole
// reason the batch run is not hammering an upstream: it asks slowly on purpose and
// must not be sped up by sharing a lease with a busier process.
func TestJobClaimsAreSpacedAtTheJobInterval(t *testing.T) {
	database := sharedTestDB(t)
	const jobInterval = 400 * time.Millisecond
	shared := NewShared(database, "lrclib", 10*time.Millisecond, jobInterval, 0)
	job := shared.ForJob()

	ctx := context.Background()
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}

	start := time.Now()
	for range 3 {
		if err := job.Wait(ctx); err != nil {
			t.Fatalf("job wait: %v", err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 2*jobInterval {
		t.Errorf("three job claims took %s, want at least %s: batch work was sped up by the live rate", elapsed, 2*jobInterval)
	}
}

// A job must never be admitted in the same second as a live request, however
// slowly the job itself paces. The idle window is what keeps a run from firing into
// a request path it is supposed to be standing aside for, and it is independent of
// either class's rate.
func TestJobStillDefersToLiveTrafficUnderASlowJobRate(t *testing.T) {
	database := sharedTestDB(t)
	shared := NewShared(database, "lrclib", 10*time.Millisecond, time.Hour, 50*time.Millisecond)
	user := shared.ForUser()
	job := shared.ForJob()

	ctx := context.Background()
	if err := user.Wait(ctx); err != nil {
		t.Fatalf("user wait: %v", err)
	}

	// The job's own interval is an hour, so anything admitted promptly here came
	// from the idle window rather than from its own rate.
	start := time.Now()
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 400*time.Millisecond {
		t.Fatalf("job waited %s after live traffic, want it admitted once the server went quiet", elapsed)
	}
}

// Live traffic must not inherit the batch rate. A server whose requests were held
// to a job's pace would be slow for its users for no reason: it is not competing
// with itself.
func TestLiveTrafficIsNotHeldToTheJobRate(t *testing.T) {
	database := sharedTestDB(t)
	shared := NewShared(database, "lrclib", 20*time.Millisecond, time.Hour, 0)
	user := shared.ForUser()

	ctx := context.Background()
	if err := user.Wait(ctx); err != nil {
		t.Fatalf("user wait: %v", err)
	}

	start := time.Now()
	if err := user.Wait(ctx); err != nil {
		t.Fatalf("user wait 2: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("a live request waited %s, want it served at the live rate rather than the job's", elapsed)
	}
}

// A caller that passes no separate job rate means "the two should share one", so
// the batch side cannot end up accidentally unspaced.
func TestAnAbsentJobIntervalFallsBackToTheLiveOne(t *testing.T) {
	database := sharedTestDB(t)
	const interval = 250 * time.Millisecond
	shared := NewShared(database, "itunes", interval, 0, 0)
	job := shared.ForJob()

	ctx := context.Background()
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}

	start := time.Now()
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("job wait 2: %v", err)
	}
	if elapsed := time.Since(start); elapsed < interval/2 {
		t.Errorf("a job claim took only %s, want it spaced at the shared %s", elapsed, interval)
	}
}

// Losing the shared lease must degrade to the rate the caller class was getting,
// not to the other class's. A server that fell back would otherwise quietly start
// pacing live requests at the batch rate, which is the failure the two-rate lease
// was introduced to avoid.
func TestDegradedModePreservesEachClassRate(t *testing.T) {
	// A nil database builds a pacer that can only pace locally, which is the
	// degraded shape: there is no lease to claim, so both classes fall back.
	const (
		userInterval = 50 * time.Millisecond
		jobInterval  = 500 * time.Millisecond
	)
	shared := NewShared(nil, "lrclib", userInterval, jobInterval, 0)
	user := shared.ForUser()
	job := shared.ForJob()

	ctx := context.Background()
	if err := user.Wait(ctx); err != nil {
		t.Fatalf("user wait: %v", err)
	}
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}

	// The live side keeps its fast rate even with the shared lease gone.
	start := time.Now()
	if err := user.Wait(ctx); err != nil {
		t.Fatalf("user wait 2: %v", err)
	}
	if elapsed := time.Since(start); elapsed > jobInterval/2 {
		t.Errorf("a degraded live request waited %s, want the live rate preserved", elapsed)
	}

	// And the job side keeps its slow one, so a broken coordination table costs
	// cross-process guarantees rather than the run's own pacing.
	start = time.Now()
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("job wait 2: %v", err)
	}
	if elapsed := time.Since(start); elapsed < jobInterval/2 {
		t.Errorf("a degraded job claim took only %s, want the job rate preserved", elapsed)
	}
}

// One lease per upstream name is shared across classes, so a job and a server
// pointed at the same provider still take turns on the same row. Distinct names
// must not interfere with each other.
func TestClassesShareOneLeaseAndNamesDoNotCollide(t *testing.T) {
	database := sharedTestDB(t)
	server := NewShared(database, "lrclib", 20*time.Millisecond, 20*time.Millisecond, time.Millisecond)
	job := NewShared(database, "lrclib", 300*time.Millisecond, 300*time.Millisecond, 0)
	other := NewShared(database, "kugou", 300*time.Millisecond, 300*time.Millisecond, 0)

	ctx := context.Background()
	if err := server.ForUser().Wait(ctx); err != nil {
		t.Fatalf("server wait: %v", err)
	}

	// A live request just used lrclib, so a job on lrclib has to stand aside for
	// the idle window even though lrclib's job rate is slow enough that it would
	// not have fired yet anyway.
	jobWaiter := job.ForJob()
	if err := jobWaiter.Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}
	if elapsed := sinceStart(t, ctx, jobWaiter); elapsed > 2*time.Second {
		t.Errorf("a job on lrclib waited %s, want it admitted once the server went quiet", elapsed)
	}

	// A different upstream is not gated on lrclib's traffic at all.
	start := time.Now()
	if err := other.ForJob().Wait(ctx); err != nil {
		t.Fatalf("other job wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a job on kugou waited %s, want it unaffected by lrclib's traffic", elapsed)
	}
}

// sinceStart is a small readability helper: it issues one more claim and reports
// how long that took.
func sinceStart(t *testing.T, ctx context.Context, waiter Waiter) time.Duration {
	t.Helper()
	start := time.Now()
	if err := waiter.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	return time.Since(start)
}
