package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
	"github.com/sillygru/music-utils/internal/pacer"
)

// metadataBackfill resolves every track that entered the library through the
// lyrics path and was therefore never looked up against a metadata provider.
//
// It exists because tracks created by a lyrics lookup carry no genre, ISRC, or
// MusicBrainz ID: nothing in the request path ever goes back and fills them in.
type metadataBackfill struct{}

// batchSize is how many resolved tracks are committed per transaction. Batching
// keeps the write lock on the shared database held briefly, so the live server
// is rarely blocked behind the backfill.
const batchSize = 64

// batchFlushInterval bounds how long a partially filled batch waits before it is
// committed, so progress near the end of a run still lands promptly.
const batchFlushInterval = 2 * time.Second

// pageSize is how many candidate tracks are read from the database at a time.
const pageSize = 500

// maxThrottleRetries is how many extra attempts a rate-limited track gets
// inside a single run.
//
// A throttle says something about the moment, not about the track, and the gate
// has already paused the whole job by the time this is consulted. Re-offering
// the track after that pause turns a transient 429 into an ordinary lookup
// instead of deferring it to a run that may be hours away. The budget is
// deliberately small: a provider still limiting after a third attempt is not
// going to answer on the fourth, and hammering it costs the whole run.
const maxThrottleRetries = 2

// throttleRetryBacklog caps how many throttled tracks may wait for a retry at
// once. It sizes the retry channel, so the backlog lives in memory for the
// length of one run and has to stay bounded; once it is full, further throttles
// are left for the next run exactly as they were before retries existed.
const throttleRetryBacklog = 4096

// writeOp is one pending database mutation, applied by the single writer.
type writeOp struct {
	work    db.MetadataWork
	track   *db.Track
	checked bool
}

func init() {
	Register(&metadataBackfill{})
}

func (j *metadataBackfill) Name() string { return "metadata-backfill" }

func (j *metadataBackfill) Summary() string {
	return "Fetch upstream metadata for tracks that were cached from lyrics only and never resolved against iTunes or Deezer"
}

func (j *metadataBackfill) Flags() string {
	return "-concurrency, -rate, -limit, -dry-run"
}

func (j *metadataBackfill) Run(ctx context.Context, opts Options) error {
	if opts.MetadataDB == nil {
		return errors.New("metadata database is required")
	}
	total, err := db.CountTracksMissingMetadata(ctx, opts.MetadataDB)
	if err != nil {
		return err
	}
	if total == 0 {
		fmt.Fprintln(opts.Out, "Nothing to do: every track already has upstream metadata.")
		return nil
	}
	planned := total
	if opts.Limit > 0 && int64(opts.Limit) < planned {
		planned = int64(opts.Limit)
	}

	workers := opts.Concurrency
	if workers < 1 {
		workers = 1
	}
	rate := opts.RatePerMinute

	fmt.Fprintf(opts.Out, "Metadata backfill\n")
	fmt.Fprintf(opts.Out, "  tracks awaiting metadata : %s\n", humanCount(planned))
	fmt.Fprintf(opts.Out, "  workers                  : %d\n", workers)
	if rate > 0 {
		fmt.Fprintf(opts.Out, "  upstream ceiling         : %d/min\n", rate)
	} else {
		fmt.Fprintf(opts.Out, "  upstream ceiling         : provider pacing only\n")
	}
	if opts.DryRun {
		fmt.Fprintf(opts.Out, "  mode                     : dry run (no writes)\n")
	}
	fmt.Fprintln(opts.Out)

	resolver, err := newBackfillResolver(opts.Config, opts.MetadataDB, opts.UserIdleGap, opts.ErrOut)
	if err != nil {
		return err
	}
	if resolver == nil {
		return errors.New("no metadata providers are enabled; check ITUNES_BASE_URL and DEEZER_BASE_URL")
	}

	lanes := make([]*Lane, workers)
	perLane := planned / int64(workers)
	remainder := planned % int64(workers)
	for i := range lanes {
		size := perLane
		if int64(i) < remainder {
			size++
		}
		lanes[i] = &Lane{Name: fmt.Sprintf("lane-%d", i+1), Total: size}
	}
	progress := opts.Progress
	if progress == nil {
		progress = NewProgress(opts.Out, lanes)
	}
	stopProgress := progress.Start()
	defer stopProgress()

	// Announce the run so the live server can see that a job is active, and
	// clear the marker on the way out however the run ends.
	//
	// Registration depends on the coordination tables, which the resolver has
	// already tried to create. Without them there is no table to update, so
	// calling BeginJob anyway would report a second, more confusing failure for
	// the same underlying cause.
	if !opts.DryRun {
		if resolver.coordinated() {
			if err := db.BeginJob(ctx, opts.MetadataDB, j.Name()); err != nil {
				fmt.Fprintf(opts.ErrOut, "register job (continuing): %v\n", err)
			} else {
				defer func() { _ = db.EndJob(context.Background(), opts.MetadataDB) }()
			}
		} else {
			fmt.Fprintf(opts.ErrOut, "job registration skipped: no coordination table, so the live server will not be told this run happened\n")
		}
	}

	gate := NewGate(rate)
	run := &backfillRun{
		opts:           opts,
		resolver:       resolver,
		gate:           gate,
		progress:       progress,
		lanes:          lanes,
		maxWriteErrors: opts.MaxWriteErrors,
		writeAbort:     make(chan struct{}),
		writeOK:        make(chan struct{}, 1),
		watchDone:      make(chan struct{}),
	}

	if err := run.execute(ctx, planned); err != nil {
		return err
	}

	rateLimits, pauses := gate.Stats()
	// A run that stopped because it could not write is not a clean finish, and
	// must not be reported as one: the operator needs to know the work is still
	// outstanding and why, without having to compare counts by hand.
	if reason := run.abort(); reason != nil {
		fmt.Fprintf(opts.ErrOut, "\nStopped early. %v\n", reason)
		fmt.Fprintf(opts.ErrOut, "%s\n", run.summary())
		return reason
	}

	fmt.Fprintf(opts.Out, "\nDone. %s\n", run.summary())
	if rateLimits > 0 {
		fmt.Fprintf(opts.Out, "Upstream rate limits: %d absorbed, %d worker pauses.\n", rateLimits, pauses)
		if retried := run.deferredCount(); retried > 0 {
			fmt.Fprintf(opts.Out, "%s rate-limited lookup(s) were retried in this run.\n", humanCount(retried))
		}
	}
	if remaining := run.remaining(ctx); remaining > 0 {
		fmt.Fprintf(opts.Out, "%s track(s) still have no upstream metadata; re-run this job to retry them.\n", humanCount(remaining))
	}
	return nil
}

// newBackfillResolver builds the same provider chain the server uses, so a
// backfilled track is stored in exactly the shape a live lookup would produce.
// The providers carry their own pacers (one request every 2s), which is the
// binding constraint; the job's gate only adds a ceiling on top.
// backfillResolver is the provider chain plus whether cross-process
// coordination was available, which decides both the pacers in use and whether
// the run can be announced to the live server.
type backfillResolver struct {
	*metadata.Resolver
	shared bool
}

// coordinated reports whether the shared upstream pacers and the job
// coordination table are usable, so a caller can skip registration rather than
// repeating a failure it already knows about.
func (b *backfillResolver) coordinated() bool { return b != nil && b.shared }

func newBackfillResolver(cfg config.Config, metadataDB *sql.DB, idleGap time.Duration, errOut io.Writer) (*backfillResolver, error) {
	timeout := time.Duration(cfg.MetadataTimeoutMS) * time.Millisecond
	var providers []metadata.Provider

	// The pacers are shared with the live server through the metadata database
	// and run in the job class, so this job only reaches an upstream during a
	// window in which no real request is pending. Live traffic always wins.
	var itunesPace pacer.Waiter = pacer.New(2 * time.Second)
	var deezerPace pacer.Waiter = pacer.New(2 * time.Second)
	coordinated := false
	if metadataDB != nil {
		if err := db.EnsureCoordination(context.Background(), metadataDB); err != nil {
			fmt.Fprintf(errOut, "shared upstream pacing unavailable, pacing locally: %v\n", err)
		} else {
			itunesPace = pacer.NewShared(metadataDB, "itunes", 2*time.Second, idleGap).ForJob()
			deezerPace = pacer.NewShared(metadataDB, "deezer", 2*time.Second, idleGap).ForJob()
			coordinated = true
		}
	}
	if itunes, err := metadata.NewITunes(cfg.ITunesBaseURL, cfg.MetadataUserAgent, timeout, itunesPace); err != nil {
		fmt.Fprintf(errOut, "skip iTunes provider: %v\n", err)
	} else {
		providers = append(providers, itunes)
	}
	if deezer, err := metadata.NewDeezer(cfg.DeezerBaseURL, cfg.MetadataUserAgent, timeout, deezerPace); err != nil {
		fmt.Fprintf(errOut, "skip Deezer provider: %v\n", err)
	} else {
		providers = append(providers, deezer)
	}
	if len(providers) == 0 {
		return nil, nil
	}
	return &backfillResolver{Resolver: metadata.NewResolver(providers...), shared: coordinated}, nil
}

// backfillRun holds the state shared by the producer, the workers, and the
// single writer goroutine.
type backfillRun struct {
	opts     Options
	resolver *backfillResolver
	gate     *Gate
	progress *Progress
	lanes    []*Lane

	writes chan writeOp
	wg     sync.WaitGroup

	inFlight atomic.Int64

	mu        sync.Mutex
	succeeded int64
	missed    int64
	throttled int64
	failed    int64
	// deferred counts throttled tracks that were handed back to the producer
	// for another attempt rather than written off.
	deferred    int64
	writeErrors int64
	// consecutiveWriteErrors counts failed batches back to back. A run that
	// cannot persist anything must stop rather than keep spending upstream
	// requests on lookups whose results are discarded.
	consecutiveWriteErrors int
	// maxWriteErrors is the abort threshold; zero or less disables the abort.
	maxWriteErrors int
	// writeAbort closes to stop the workers once the threshold is reached.
	writeAbort chan struct{}
	// abortOnce guards closing writeAbort.
	abortOnce sync.Once
	// watchDone closes when the run ends for any reason, releasing the
	// watcher goroutine. A run that finishes cleanly never closes writeAbort,
	// so without this the watcher outlives every successful run.
	watchDone chan struct{}
	// abortReason explains why the run stopped, reported in the summary so a
	// partial run is never mistaken for a clean finish.
	abortReason error
	// writeOK records a successful commit, which resets the consecutive count.
	writeOK chan struct{}

	// retryMu guards retryAttempts. It is deliberately separate from mu so a
	// worker queueing a retry never waits behind the summary counters.
	retryMu sync.Mutex
	// retryChan carries throttled tracks back to whichever worker frees up
	// first. Its capacity is throttleRetryBacklog, which is what bounds the
	// backlog.
	retryChan chan db.MetadataWork
	// retryAttempts counts the retries already granted to each track, so
	// maxThrottleRetries is enforced per track rather than per run. It is
	// created on first use so a directly constructed run needs no setup.
	retryAttempts map[int64]int
}

func (r *backfillRun) execute(ctx context.Context, planned int64) error {
	// runCtx is cancelled internally once the workers stop, so the caller's
	// context is what decides whether the run ended in error.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Write failures stop the run from inside the writer, so the workers have
	// to observe it. Deriving their context here means cancellation propagates
	// to the gate and the pacers as an ordinary context error, and the producer
	// unwinds through the same path as an operator interrupt.
	workCtx, stopWork := context.WithCancel(runCtx)
	defer stopWork()
	watcherDone := make(chan struct{})
	// Release the watcher on every exit path, not just an abort. It is a
	// single goroutine per run, and this job is built to be callable in a
	// loop, so leaking one per successful run would accumulate.
	defer func() {
		close(r.watchDone)
		<-watcherDone
	}()
	r.watchWrites(stopWork, watcherDone)

	r.writes = make(chan writeOp, batchSize*2)
	// Sized once per run so the retry backlog is bounded for the whole pass.
	r.retryChan = make(chan db.MetadataWork, throttleRetryBacklog)

	// One writer owns every mutation. Funneling writes through a single
	// goroutine keeps the commit path serialized and bounded, which is what
	// makes it safe to run alongside the live server.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		// The writer deliberately runs on the caller's context, not runCtx:
		// cancelling runCtx below only stops fetching, and the batch already
		// resolved still has to be committed.
		r.writeLoop(ctx)
	}()

	work := make(chan db.MetadataWork)
	var workers sync.WaitGroup
	for i := range r.lanes {
		workers.Add(1)
		go func(lane *Lane) {
			defer workers.Done()
			r.worker(workCtx, lane, work)
		}(r.lanes[i])
	}

	// The producer streams pages so a library of any size runs in constant
	// memory. It stops early on cancellation or once the planned cap is met.
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(work)
		r.produce(workCtx, work, planned)
	}()

	workers.Wait()
	// Unblock the producer, which may still be waiting to hand off a track.
	cancel()
	<-producerDone
	// Every worker has stopped, so nothing else can enqueue a write. Closing
	// the channel lets the writer flush its final batch and exit on its own
	// terms rather than being torn down mid-transaction.
	close(r.writes)
	<-writerDone
	return ctx.Err()
}

func (r *backfillRun) produce(ctx context.Context, out chan<- db.MetadataWork, planned int64) {
	var afterID int64
	var produced int64
	for produced < planned {
		if ctx.Err() != nil {
			return
		}
		page, err := db.ListTracksMissingMetadata(ctx, r.opts.MetadataDB, afterID, pageSize)
		if err != nil {
			if ctx.Err() == nil {
				r.progress.Notice("read tracks: %v", err)
			}
			return
		}
		if len(page) == 0 {
			return
		}
		for _, item := range page {
			select {
			case <-ctx.Done():
				return
			case out <- item:
			}
			r.inFlight.Add(1)
			produced++
			afterID = item.ID
			if produced >= planned {
				return
			}
		}
	}
}

// retryLater hands a throttled track back for another attempt, reporting
// whether the retry was granted.
//
// The track goes onto the retry channel, which workers select on alongside the
// main work stream. That is what makes a retry immediate: a worker that is
// between tracks picks the track straight back up, and the gate has already
// paused it, so the lookup happens as soon as the cooldown expires rather than
// at the end of a twelve-hour pass.
//
// A track is retried only while it has budget left and the buffer has room, so
// a provider that throttles the same song repeatedly cannot spin the run, and a
// run that throttles everything at once cannot grow without bound.
func (r *backfillRun) retryLater(item db.MetadataWork) bool {
	r.retryMu.Lock()
	if r.retryAttempts == nil {
		r.retryAttempts = make(map[int64]int)
	}
	if r.retryAttempts[item.ID] >= maxThrottleRetries {
		r.retryMu.Unlock()
		return false
	}
	// The send is non-blocking: a full buffer means the backlog is at its
	// bound, and the track is left for the next run rather than growing this
	// one's memory. The attempt is only charged when the retry really queued.
	select {
	case r.retryChan <- item:
		r.retryAttempts[item.ID]++
	default:
		r.retryMu.Unlock()
		return false
	}
	r.retryMu.Unlock()

	r.mu.Lock()
	r.deferred++
	r.mu.Unlock()
	return true
}

// worker resolves one track at a time. Each worker owns a lane so the rendered
// output shows real per-lane progress rather than one opaque number.
//
// It takes from two streams: the library the producer is walking, and the retry
// channel that throttled tracks are re-offered on. Taking from both is what
// makes a throttled track get another attempt seconds later instead of after
// the whole pass.
func (r *backfillRun) worker(ctx context.Context, lane *Lane, work <-chan db.MetadataWork) {
	for {
		var item db.MetadataWork
		if work != nil {
			select {
			case <-ctx.Done():
				return
			case next, ok := <-work:
				if !ok {
					work = nil
					continue
				}
				item = next
			case item = <-r.retryChan:
			}
		} else {
			if r.inFlight.Load() <= 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			case item = <-r.retryChan:
			case <-time.After(10 * time.Millisecond):
				if r.inFlight.Load() <= 0 {
					return
				}
				continue
			}
		}
		if !r.resolveOne(ctx, lane, item) {
			return
		}
	}
}

// resolveOne looks up a single track and records the outcome, reporting whether
// the worker should keep going.
func (r *backfillRun) resolveOne(ctx context.Context, lane *Lane, item db.MetadataWork) bool {
	label := item.Name
	if item.Artist != "" {
		label += " - " + item.Artist
	}

	if err := r.gate.Wait(ctx); err != nil {
		return false
	}
	track, err := r.resolver.Lookup(ctx, metadata.Input{
		TrackName:  item.Name,
		ArtistName: item.Artist,
		AlbumName:  item.Album,
		Duration:   item.Duration,
	})

	outcome := classifyLookup(track, err)
	switch outcome {
	case OutcomeSucceeded:
		r.gate.Succeed()
		r.progress.Advance(lane, outcome, label)
		r.enqueue(ctx, writeOp{work: item, track: track})
		r.inFlight.Add(-1)
	case OutcomeMissing:
		r.gate.Succeed()
		r.progress.Advance(lane, outcome, label)
		r.enqueue(ctx, writeOp{work: item, checked: true})
		r.inFlight.Add(-1)
	case OutcomeThrottled:
		r.gate.Throttle(metadata.RetryAfterFor(err))
		// The track goes back on the retry channel for another attempt, so it
		// is deliberately not advanced: it is still in flight, and counting it
		// now would let a lane overshoot its share of the plan.
		if r.retryLater(item) {
			return true
		}
		// The retry budget is spent, or the backlog is full, so this is the
		// track's final answer for this run. It stays unchecked in the database
		// and is picked up again next time.
		r.inFlight.Add(-1)
		r.countOutcome(outcome)
		r.progress.Advance(lane, outcome, label)
	default:
		r.inFlight.Add(-1)
		r.countOutcome(OutcomeFailed)
		r.progress.Advance(lane, outcome, label)
		if ctx.Err() == nil {
			r.progress.Notice("lookup %q: %v", label, err)
		}
	}
	// A run that cannot persist anything must not keep spending upstream
	// requests. The context is cancelled by the writer; returning here
	// leaves the remaining tracks for the next run, which is where they
	// would have ended up regardless.
	return !r.writeFailed()
}

// classifyLookup decides how a lookup result is counted, and by extension
// whether the track may be recorded as resolved.
//
// Only OutcomeMissing is persisted as a negative. A throttle, a provider error,
// a timeout, or a network failure all leave the track unchecked so a later run
// retries it; marking any of them resolved would permanently record that a
// song has no upstream data on the strength of a bad moment.
func classifyLookup(track *db.Track, err error) Outcome {
	switch {
	// A track in hand always wins, even if an error came back with it: merging
	// the metadata is strictly better than recording the track as resolved
	// without it, which would throw the data away.
	case track != nil:
		return OutcomeSucceeded
	case metadata.IsRateLimited(err):
		return OutcomeThrottled
	case errors.Is(err, metadata.ErrInconclusive):
		return OutcomeFailed
	case errors.Is(err, metadata.ErrNotFound):
		return OutcomeMissing
	default:
		return OutcomeFailed
	}
}

// countOutcome tallies outcomes that never reach the database. Resolved and
// missed tracks are counted in commit instead, so the final summary reports
// what was actually persisted rather than what was merely looked up: a run
// interrupted mid-flight drops queued writes, and those tracks are retried on
// the next pass.
func (r *backfillRun) countOutcome(outcome Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch outcome {
	case OutcomeThrottled:
		r.throttled++
	case OutcomeFailed:
		r.failed++
	}
}

// countCommitted tallies a batch that was durably written.
func (r *backfillRun) countCommitted(batch []writeOp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, op := range batch {
		if op.checked {
			r.missed++
		} else {
			r.succeeded++
		}
	}
}

func (r *backfillRun) enqueue(ctx context.Context, op writeOp) {
	select {
	case r.writes <- op:
	case <-ctx.Done():
	}
}

// writeLoop commits pending operations in batches. A batch is flushed when it
// fills, when the flush interval elapses, or when the run ends, so a partially
// filled batch is never left uncommitted.
func (r *backfillRun) writeLoop(ctx context.Context) {
	batch := make([]writeOp, 0, batchSize)
	ticker := time.NewTicker(batchFlushInterval)
	defer ticker.Stop()

	flush := func(flushCtx context.Context) {
		if len(batch) == 0 {
			return
		}
		r.commit(flushCtx, batch)
		batch = batch[:0]
		// Keep the frame's failure count current even between commits, so an
		// abort in progress is visible rather than only in the final summary.
		r.progress.SetWriteErrors(r.writeErrorCount())
	}

	for {
		select {
		case op, ok := <-r.writes:
			if !ok {
				flush(ctx)
				return
			}
			batch = append(batch, op)
			if len(batch) >= batchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-ctx.Done():
			// The caller is interrupting. Commit what is already resolved using
			// a detached context: the run context is cancelled, so reusing it
			// here would fail to open a transaction and silently discard the
			// final partial batch. A short bound keeps shutdown from hanging on
			// a database lock held by the live server.
			detach, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			flush(detach)
			cancel()
			return
		}
	}
}

// commit applies one batch inside a single transaction. Either every track in
// the batch is updated or none is, so an interrupted run never leaves a
// half-applied batch.
func (r *backfillRun) commit(ctx context.Context, batch []writeOp) {
	if r.opts.DryRun {
		return
	}
	tx, err := r.opts.MetadataDB.BeginTx(ctx, nil)
	if err != nil {
		r.noteWriteError(err)
		r.reportWriteError("begin batch", err)
		return
	}
	for _, op := range batch {
		if op.checked {
			err = db.MarkTrackMetadataChecked(ctx, tx, op.work.ID)
		} else {
			merged := *op.track
			merged.ID = op.work.ID
			merged.MetadataChecked = true
			err = db.MergeTrackMetadata(ctx, tx, op.work.ID, merged)
		}
		if err != nil {
			_ = tx.Rollback()
			r.noteWriteError(err)
			r.reportWriteError("commit batch", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		r.noteWriteError(err)
		r.reportWriteError("commit batch", err)
		return
	}
	r.countCommitted(batch)
	r.noteWriteSuccess()
	// Tell the live server its memoized metadata is stale. A failure here only
	// costs the server a stale entry until it restarts, so it must not fail the
	// run.
	if _, err := db.BumpMetadataRevision(ctx, r.opts.MetadataDB); err != nil && ctx.Err() == nil {
		r.progress.Notice("signal metadata revision: %v", err)
	}
}

func (r *backfillRun) noteWriteError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeErrors++
	r.consecutiveWriteErrors++
	reached := r.maxWriteErrors > 0 && r.consecutiveWriteErrors >= r.maxWriteErrors
	if reached {
		r.abortReason = fmt.Errorf("aborted after %d consecutive failed write batches (last error: %w)",
			r.consecutiveWriteErrors, err)
	}
	if !reached {
		return
	}
	// Stop the workers while the writer is still on the caller's context, so
	// the run unwinds through the normal path and the final batch is flushed.
	r.abortOnce.Do(func() { close(r.writeAbort) })
}

// writeErrorCount reports the total number of failed write batches.
func (r *backfillRun) writeErrorCount() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeErrors
}

// writeFailed reports whether the writer has given up, so a worker can stop
// early instead of running a full lookups-only pass to the end of the queue.
func (r *backfillRun) writeFailed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abortReason != nil
}

// abort returns the reason the run stopped early, or nil.
func (r *backfillRun) abort() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abortReason
}

// reportWriteError records a write failure for the frame and prints it. The
// message goes through the renderer so it cannot be swallowed by an in-place
// redraw that is happening at the same moment.
func (r *backfillRun) reportWriteError(stage string, err error) {
	r.progress.Notice("%s: %v", stage, err)
	if reason := r.abort(); reason != nil {
		r.progress.Notice("%v", reason)
	}
}

// noteWriteSuccess resets the consecutive failure streak, so a run that hits a
// busy database a few times is not aborted by failures that are no longer
// happening.
func (r *backfillRun) noteWriteSuccess() {
	r.mu.Lock()
	had := r.consecutiveWriteErrors > 0
	r.consecutiveWriteErrors = 0
	r.mu.Unlock()
	if !had {
		return
	}
	// Signal without blocking: the watcher is a single goroutine and a pending
	// reset is equivalent to a delivered one.
	select {
	case r.writeOK <- struct{}{}:
	default:
	}
}

// watchWrites cancels the workers' context once the writer decides the run
// cannot continue. It also drains success signals so a recovered streak is
// noticed promptly.
//
// It exits on writeAbort or when the run ends, whichever comes first, and closes
// done on the way out so the caller can wait for it. A run that never aborts
// still has to release this goroutine: the only signal an abort raises is
// writeAbort, so without the run-end signal the watcher would outlive every
// successful run.
func (r *backfillRun) watchWrites(stopWork context.CancelFunc, done chan<- struct{}) {
	go func() {
		defer close(done)
		for {
			select {
			case <-r.writeAbort:
				stopWork()
				return
			case <-r.watchDone:
				return
			case <-r.writeOK:
				// A successful batch already cleared the streak; nothing to do
				// beyond keeping the signal drained.
			}
		}
	}()
}

// deferredCount reports how many throttled tracks were retried in this run
// rather than left for the next one.
func (r *backfillRun) deferredCount() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deferred
}

// remaining reports how many planned tracks are still unresolved, so the
// summary can tell the operator whether a re-run would do anything.
func (r *backfillRun) remaining(ctx context.Context) int64 {
	if r.opts.DryRun {
		return 0
	}
	total, err := db.CountTracksMissingMetadata(ctx, r.opts.MetadataDB)
	if err != nil {
		return 0
	}
	return total
}

func (r *backfillRun) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := []string{
		fmt.Sprintf("%s resolved", humanCount(r.succeeded)),
		fmt.Sprintf("%s not found upstream", humanCount(r.missed)),
	}
	if r.throttled > 0 {
		parts = append(parts, fmt.Sprintf("%s rate limited", humanCount(r.throttled)))
	}
	if r.deferred > 0 {
		parts = append(parts, fmt.Sprintf("%s rate limited and retried", humanCount(r.deferred)))
	}
	if r.failed > 0 {
		parts = append(parts, fmt.Sprintf("%s failed", humanCount(r.failed)))
	}
	if r.writeErrors > 0 {
		parts = append(parts, fmt.Sprintf("%s write errors", humanCount(r.writeErrors)))
	}
	return strings.Join(parts, ", ") + "."
}
