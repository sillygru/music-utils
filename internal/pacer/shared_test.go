package pacer

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

// sharedTestDB opens a file-backed metadata database with the coordination
// tables. A file is required rather than :memory: because the point of these
// tests is that separate connections, standing in for separate processes, take
// turns.
func sharedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "coordination.db"),
		// TxLockImmediate must match how the server and the job open the
		// metadata database. Without it the lease races on a deferred
		// transaction and these tests would validate a configuration that is
		// never actually deployed.
		db.Config{MmapSize: 64 * 1024 * 1024, CacheSizeKB: -2000, MaxOpenConns: 4, TxLockImmediate: true})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.EnsureCoordination(context.Background(), database); err != nil {
		t.Fatalf("ensure coordination: %v", err)
	}
	return database
}

// TestUserTrafficIsServedImmediately is the core promise: a live request is
// never made to queue behind a background job that claimed a slot.
func TestUserTrafficIsServedImmediately(t *testing.T) {
	database := sharedTestDB(t)
	shared := NewShared(database, "itunes", 2*time.Second, 2*time.Second, 10*time.Second)

	// A job claims a far-future slot, as it would while queued behind traffic.
	if _, err := shared.claim(context.Background(), ClassJob); err != nil {
		t.Fatalf("job claim: %v", err)
	}
	// Push the shared lease out so the job is definitively occupying a later
	// slot. Live traffic must ignore it entirely.
	if _, err := database.Exec(`UPDATE upstream_pacer SET next_at = ? WHERE name = 'itunes'`,
		time.Now().Add(30*time.Second).UnixMilli()); err != nil {
		t.Fatalf("push lease: %v", err)
	}

	start := time.Now()
	if err := shared.ForUser().Wait(context.Background()); err != nil {
		t.Fatalf("user wait: %v", err)
	}
	// The user must not be made to wait out the job's slot.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("live request waited %s behind a job slot", elapsed)
	}
}

// TestJobWaitsForIdleWindow checks the priority inversion that motivates this
// whole mechanism: after a user request, background work stands aside.
func TestJobWaitsForIdleWindow(t *testing.T) {
	database := sharedTestDB(t)
	const idleGap = 3 * time.Second
	shared := NewShared(database, "itunes", 0, 0, idleGap)

	// Live traffic just happened.
	if err := shared.ForUser().Wait(context.Background()); err != nil {
		t.Fatalf("user wait: %v", err)
	}

	start := time.Now()
	// The job must refuse to fire while live traffic is recent, and must not
	// fire before the idle window it is waiting out.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := shared.ForJob().Wait(ctx); err == nil {
		t.Fatal("expected the job to wait out the idle window instead of being admitted")
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("job should not have waited past the idle window, waited %s", elapsed)
	}
}

// TestJobStepsAsideForUserArrivingMidWait is the priority guarantee that a job
// already holding a reserved slot still yields to live traffic.
//
// The reservation was computed from the traffic present at claim time, so it is
// stale by the time the wait ends. Only a fresh read distinguishes "the server
// stayed quiet" from "a user turned up while I was asleep". The test therefore
// has to interrupt an actual wait: the lease is primed first so the job has to
// sleep, and the user arrives inside that sleep.
func TestJobStepsAsideForUserArrivingMidWait(t *testing.T) {
	database := sharedTestDB(t)
	const (
		interval = 300 * time.Millisecond
		idleGap  = time.Second
	)
	shared := NewShared(database, "itunes", interval, interval, idleGap)

	// Prime the lease so the job's own claim resolves to a slot in the future
	// and it genuinely has to wait. No user traffic is recorded, so without a
	// user arriving the job would be admitted the moment this wait ends.
	if _, err := shared.claim(context.Background(), ClassJob); err != nil {
		t.Fatalf("prime lease: %v", err)
	}

	ctx := context.Background()
	admitted := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		if err := shared.ForJob().Wait(ctx); err != nil {
			admitted <- 0
			return
		}
		admitted <- time.Since(start)
	}()

	// A live request lands while the job is mid-wait.
	time.Sleep(interval / 2)
	userAt := time.Now()
	if err := shared.ForUser().Wait(ctx); err != nil {
		t.Fatalf("user wait: %v", err)
	}

	elapsed := <-admitted
	if elapsed == 0 {
		t.Fatal("job wait failed")
	}
	// Re-admitting on the stale snapshot lands here roughly one interval after
	// the claim, so the tolerance below is only tight enough to exclude that
	// outcome; the correct admission sits a whole idle window after the user.
	if sinceUser := time.Since(userAt); sinceUser < idleGap-100*time.Millisecond {
		t.Fatalf("job was admitted %s after a live request, without waiting out the %s idle window",
			sinceUser, idleGap)
	}
	if elapsed < idleGap {
		t.Fatalf("job was admitted %s after starting, faster than the %s idle window", elapsed, idleGap)
	}
}

// TestJobReadFailureStandsAside: if the lease cannot be re-read, the job has no
// evidence the server is idle. Standing aside is the conservative reading, so a
// transient read error must not become a licence to fire.
func TestJobReadFailureStandsAside(t *testing.T) {
	database := sharedTestDB(t)
	shared := NewShared(database, "itunes", 0, 0, 50*time.Millisecond)

	if err := shared.ForJob().Wait(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Drop the lease table so the post-wait re-read fails. The next claim also
	// fails, which routes the job to local pacing; the point is that it is not
	// admitted on the strength of an unreadable lease.
	if _, err := database.Exec("DROP TABLE upstream_pacer"); err != nil {
		t.Fatalf("drop lease table: %v", err)
	}

	if _, err := shared.readLastUserAt(context.Background()); err == nil {
		t.Fatal("expected the re-read to fail once the table is gone")
	}
	// A failed read must be reported as "busy now", which the caller can verify
	// through idleFor: a timestamp of now means not idle.
	now := time.Now().UnixMilli()
	if shared.idleFor(now) {
		t.Fatal("a fresh timestamp must not count as an idle window")
	}
	shared.disable()
	if err := shared.ForJob().Wait(context.Background()); err != nil {
		t.Fatalf("job should fall back to local pacing, got: %v", err)
	}
}

// TestJobStopsWaitingOutAnIdleWindowItWillNeverGet covers the starvation that
// made a backfill sit at 0% forever: a server with steady traffic never reaches
// the idle window, so a job that yields unconditionally never runs.
func TestJobStopsWaitingOutAnIdleWindowItWillNeverGet(t *testing.T) {
	database := sharedTestDB(t)
	shared := NewShared(database, "itunes", 0, 0, 2*time.Second)

	// Live traffic keeps landing, so every idle check fails.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := shared.ForUser().Wait(context.Background()); err != nil {
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()

	ctx := context.Background()
	start := time.Now()
	if err := shared.ForJob().Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}
	// It may defer first, but only within the budget, and it cannot be
	// unbounded: the real deployment's symptom was a run that never moved.
	if elapsed := time.Since(start); elapsed > maxIdleWait+2*time.Second {
		t.Fatalf("job waited %s for an idle window it can never get", elapsed)
	}
}

func TestJobProceedsWhenServerIsIdle(t *testing.T) {
	database := sharedTestDB(t)
	// A tiny gap keeps the test quick while still exercising the same path.
	shared := NewShared(database, "itunes", 0, 0, 50*time.Millisecond)

	// No user traffic has ever been recorded.
	start := time.Now()
	if err := shared.ForJob().Wait(context.Background()); err != nil {
		t.Fatalf("job wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected an idle server to admit the job promptly, waited %s", elapsed)
	}
}

// TestCombinedTrafficIsSpaced proves the original problem is fixed: a job and
// the server sharing one lease cannot together exceed the configured rate.
func TestCombinedTrafficIsSpaced(t *testing.T) {
	database := sharedTestDB(t)
	const interval = 300 * time.Millisecond
	server := NewShared(database, "itunes", interval, interval, time.Duration(0))
	job := NewShared(database, "itunes", interval, interval, 1*time.Nanosecond)

	serverWaiter := server.ForUser()
	jobWaiter := job.ForJob()

	ctx := context.Background()
	// Prime the lease; the first claim is free on both sides.
	if err := serverWaiter.Wait(ctx); err != nil {
		t.Fatalf("server wait: %v", err)
	}
	if err := jobWaiter.Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}

	// Now measure two alternating claims against the shared lease.
	start := time.Now()
	if err := serverWaiter.Wait(ctx); err != nil {
		t.Fatalf("server wait 2: %v", err)
	}
	if err := jobWaiter.Wait(ctx); err != nil {
		t.Fatalf("job wait 2: %v", err)
	}
	elapsed := time.Since(start)
	// Two claims spaced 300ms apart must consume at least one full interval
	// between them; without the shared lease each side would pace privately and
	// both would return almost immediately.
	if elapsed < 200*time.Millisecond {
		t.Fatalf("expected combined pacing across processes, two claims took only %s", elapsed)
	}
}

// TestSeparateInstancesShareOneLease simulates two processes: distinct Shared
// values over one database must not both claim the same instant.
func TestSeparateInstancesShareOneLease(t *testing.T) {
	database := sharedTestDB(t)
	const interval = 200 * time.Millisecond
	first := NewShared(database, "itunes", interval, interval, 0)
	second := NewShared(database, "itunes", interval, interval, 0)

	ctx := context.Background()
	var mu sync.Mutex
	var stamps []time.Time
	var wait sync.WaitGroup
	for _, shared := range []*Shared{first, second} {
		wait.Add(1)
		go func(s *Shared) {
			defer wait.Done()
			for i := 0; i < 3; i++ {
				if err := s.ForUser().Wait(ctx); err != nil {
					return
				}
				mu.Lock()
				stamps = append(stamps, time.Now())
				mu.Unlock()
			}
		}(shared)
	}
	wait.Wait()

	if len(stamps) != 6 {
		t.Fatalf("expected 6 claims, got %d", len(stamps))
	}
	for _, shared := range []*Shared{first, second} {
		if degraded, reason := shared.Degraded(); degraded {
			t.Fatalf("shared claim failed: %v", reason)
		}
	}
	// Sort and confirm consecutive claims are at least ~interval apart, which
	// can only happen if both goroutines contended on the same row.
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < 150*time.Millisecond {
			t.Fatalf("claim %d came %s after the previous, expected >= %s: lease not shared",
				i, gap, interval)
		}
	}
}

// TestSharedRecoversAfterTransientFailure: one failed claim must not cost the
// process the cross-process guarantee for the rest of its life. Reverting to
// private pacing silently reintroduces the doubled request rate that shared
// pacing exists to prevent, so the lease has to be retried after a cooldown.
func TestSharedRecoversAfterTransientFailure(t *testing.T) {
	database := sharedTestDB(t)
	shared := NewShared(database, "itunes", 0, 0, 0)
	ctx := context.Background()

	// A write failure, as a database held by the live server would produce.
	if _, err := shared.claim(ctx, ClassUser); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if _, err := database.Exec("DROP TABLE upstream_pacer"); err != nil {
		t.Fatalf("drop lease table: %v", err)
	}
	// Force the cooldown to have already elapsed.
	shared.mu.Lock()
	shared.disabledUntil = time.Now().Add(-time.Second)
	shared.mu.Unlock()

	// The table is genuinely gone, so the retry fails too and the pacer stays
	// degraded. That is the correct outcome: the point is that it retried at all.
	if err := shared.ForUser().Wait(ctx); err != nil {
		t.Fatalf("user wait: %v", err)
	}
	if degraded, reason := shared.Degraded(); !degraded || reason == nil {
		t.Error("expected a permanently broken lease to keep reporting itself degraded")
	}

	// Restore the table and expire the cooldown: the pacer must recover on its
	// own, with no restart.
	if err := db.EnsureCoordination(ctx, database); err != nil {
		t.Fatalf("restore coordination: %v", err)
	}
	shared.mu.Lock()
	shared.disabledUntil = time.Now().Add(-time.Second)
	shared.mu.Unlock()

	if err := shared.ForUser().Wait(ctx); err != nil {
		t.Fatalf("user wait after recovery: %v", err)
	}
	if degraded, reason := shared.Degraded(); degraded {
		t.Errorf("pacer did not recover once the lease worked again: %v", reason)
	}
}

func TestSharedFallsBackToLocalPacing(t *testing.T) {
	// A nil database means no coordination table; the job must still pace
	// locally rather than fire unthrottled.
	shared := NewShared(nil, "itunes", 50*time.Millisecond, 50*time.Millisecond, 0)

	ctx := context.Background()
	if err := shared.ForJob().Wait(ctx); err != nil {
		t.Fatalf("job wait: %v", err)
	}
	start := time.Now()
	if err := shared.ForJob().Wait(ctx); err != nil {
		t.Fatalf("job wait 2: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("expected local fallback pacing, waited %s", elapsed)
	}
}

func TestSharedWaitHonorsContextCancellation(t *testing.T) {
	database := sharedTestDB(t)
	shared := NewShared(database, "itunes", time.Hour, time.Hour, 0)
	// Claim once so the next call must wait a full hour.
	if err := shared.ForUser().Wait(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := shared.ForUser().Wait(ctx); err == nil {
		if degraded, reason := shared.Degraded(); degraded {
			t.Fatalf("shared claim failed: %v", reason)
		}
		t.Fatal("expected Wait to honor cancellation")
	}
}

func TestEnsureCoordinationIsIdempotent(t *testing.T) {
	database := sharedTestDB(t)
	ctx := context.Background()
	// Must be safe to call repeatedly from the server and a job at once.
	for i := 0; i < 3; i++ {
		if err := db.EnsureCoordination(ctx, database); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	state, err := db.ReadCoordination(ctx, database)
	if err != nil {
		t.Fatalf("read coordination: %v", err)
	}
	if state.MetadataRevision != 0 || state.ActiveJob != "" {
		t.Fatalf("expected a fresh idle state, got %+v", state)
	}
}

func TestRevisionAndJobStateRoundTrip(t *testing.T) {
	database := sharedTestDB(t)
	ctx := context.Background()

	if err := db.BeginJob(ctx, database, "metadata-backfill"); err != nil {
		t.Fatalf("begin job: %v", err)
	}
	state, err := db.ReadCoordination(ctx, database)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if state.ActiveJob != "metadata-backfill" {
		t.Fatalf("expected the active job to be visible, got %q", state.ActiveJob)
	}
	if !state.JobStartedAt.Valid {
		t.Error("expected a job start time")
	}

	first, err := db.BumpMetadataRevision(ctx, database)
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	second, err := db.BumpMetadataRevision(ctx, database)
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	if second != first+1 {
		t.Fatalf("expected the revision to advance by one, got %d then %d", first, second)
	}

	if err := db.EndJob(ctx, database); err != nil {
		t.Fatalf("end job: %v", err)
	}
	state, err = db.ReadCoordination(ctx, database)
	if err != nil {
		t.Fatalf("read after end: %v", err)
	}
	if state.ActiveJob != "" {
		t.Errorf("expected the job marker to be cleared, got %q", state.ActiveJob)
	}
	// Ending a job must not reset the revision, or the server would miss writes.
	if state.MetadataRevision != second {
		t.Errorf("ending a job changed the revision: %d, want %d", state.MetadataRevision, second)
	}
}

func TestRevisionBumpsAreThrottledButNeverLost(t *testing.T) {
	database := sharedTestDB(t)
	ctx := context.Background()

	first, err := db.BumpMetadataRevision(ctx, database)
	if err != nil {
		t.Fatalf("first bump: %v", err)
	}
	state, err := db.ReadCoordination(ctx, database)
	if err != nil {
		t.Fatalf("read after first bump: %v", err)
	}
	if state.RevisionBumpedAt <= 0 {
		t.Fatal("expected the first bump to publish a notification stamp")
	}
	firstStamp := state.RevisionBumpedAt

	// Further bumps inside the window must still advance the counter. Losing
	// them would mean a later reader comparing revisions could not tell how far
	// the job had got, and a restart mid-run would look like no work happened.
	for i := 0; i < 3; i++ {
		latest, err := db.BumpMetadataRevision(ctx, database)
		if err != nil {
			t.Fatalf("bump %d: %v", i, err)
		}
		if latest != first+int64(i)+1 {
			t.Errorf("bump %d: revision = %d, want %d", i, latest, first+int64(i)+1)
		}
	}

	// The stamp is what the server watches, so it must hold still inside the
	// throttle window. Without this the server drops its whole cache on every
	// poll for the duration of a long backfill, which defeats a 24h miss cache.
	state, err = db.ReadCoordination(ctx, database)
	if err != nil {
		t.Fatalf("read after throttled bumps: %v", err)
	}
	if state.RevisionBumpedAt != firstStamp {
		t.Errorf("expected the notification stamp to hold at %d, got %d",
			firstStamp, state.RevisionBumpedAt)
	}
	// The counter must have moved on regardless, so the coalescing is visible
	// as a divergence rather than as lost work.
	if state.MetadataRevision != first+3 {
		t.Errorf("expected revision %d, got %d", first+3, state.MetadataRevision)
	}
}
