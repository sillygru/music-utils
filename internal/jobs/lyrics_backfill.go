package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

// lyricsBackfill fills in the tracks that entered the library without lyrics and
// were therefore never asked for any.
//
// The metadata backfill's counterpart for lyrics. The two are separate jobs
// because they answer different questions: a track can have a genre and no
// lyrics, or lyrics and no genre, and resolving one tells you nothing about the
// other.
type lyricsBackfill struct{}

// lyricsWriteBatch is how many resolved tracks are committed per transaction.
// Smaller than the metadata batch because each op here also writes to the lyrics
// database, so a batch holds its statements open longer.
const lyricsWriteBatch = 32

// lyricsBatchFlushInterval bounds how long a partially filled batch waits before
// it is committed, so progress near the end of a run still lands promptly.
const lyricsBatchFlushInterval = 2 * time.Second

// lyricsRejectionPause is how long a slot sits out after a provider refuses it.
// The refusal is usually a short block or a rate limit that tripped a stricter
// threshold, so waiting costs a minute instead of ending the run.
const lyricsRejectionPause = 60 * time.Second

// lyricsProbeTimeout bounds the startup writability check. It normally completes
// in milliseconds, but the probe takes a write lock, so a lyrics database held
// under a long transaction by the live server must report as unusable rather than
// making the job look like it has hung.
const lyricsProbeTimeout = 5 * time.Second

// lyricsWriteOp is one pending mutation for the single writer.
//
// The unit of work is a song paired with one provider, so a song produces several
// of these rather than one: an answer is written as soon as its provider gives
// it, and a separate op settles the song once every provider has replied. A run
// interrupted in the middle therefore keeps every answer it did get, which is the
// opposite of how a fan-out behaves, where an interruption before the last
// provider returns loses the whole track.
type lyricsWriteOp struct {
	work db.LyricsWork
	// answer is one provider's usable answer, written the moment it arrives.
	answer *lyricsHit
	// missed names a provider that gave a definitive negative.
	missed string
	// recordMiss says whether that negative may be filed in the ledger. The ledger
	// is keyed by track alone, so a miss stored under a partial identity would
	// wrongly suppress a later retry made once the artist or album is known.
	recordMiss bool
	// settle marks the op that finishes a song: it points the track at the best
	// lyrics stored for it and records that every provider answered.
	settle bool
	// settleUnknownAge marks a track that already had usable lyrics cached. It is
	// stamped settled without an upstream request, which is the common case on a
	// library that predates these columns and is what lets the first run finish
	// without re-fetching a library that is already whole.
	settleUnknownAge bool
}

func init() {
	Register(&lyricsBackfill{})
}

func (j *lyricsBackfill) Name() string { return "lyrics-backfill" }

func (j *lyricsBackfill) Summary() string {
	return "Fetch upstream lyrics for tracks that have none, optionally refreshing ones a provider already answered. Every provider is asked about every song, one song at a time per provider, and each answer is stored as it arrives; the best one is served. Plain and synced lyrics only: YouTube needs a video ID and the word-sync providers are left to the server"
}

func (j *lyricsBackfill) Flags() string {
	return "-concurrency, -rate, -limit, -dry-run, -refresh"
}

// FlagDescriptions restates the two shared flags whose units differ here from the
// metadata job's.
//
// The lyrics job keeps whole songs in flight rather than independent workers, and
// -rate still counts songs rather than the requests they spend, so the generic
// wording would describe neither correctly.
func (j *lyricsBackfill) FlagDescriptions() map[string]string {
	return map[string]string{
		"concurrency": "songs worked on at once, spread across providers",
		"rate":        "songs started per minute (0 = per-provider pacing only)",
	}
}

func (j *lyricsBackfill) Run(ctx context.Context, opts Options) error {
	if opts.MetadataDB == nil {
		return errors.New("metadata database is required")
	}
	if opts.LyricsDB == nil {
		return errors.New("lyrics database is required (set LYRICS_DB_PATH or pass -lyrics)")
	}
	// Probing here rather than relying on the command layer's check is what keeps
	// this job honest about the database it actually writes to. A run that cannot
	// persist anything is not a slow run, it is a no-op that has already spent the
	// whole upstream budget finding that out, and the operator needs the file name
	// rather than a page of "commit batch: attempt to write a readonly database".
	//
	// A dry run writes nothing by design, so it is exempt.
	if !opts.DryRun {
		probeCtx, cancelProbe := context.WithTimeout(ctx, lyricsProbeTimeout)
		err := db.ProbeWritable(probeCtx, opts.LyricsDB)
		cancelProbe()
		if err != nil {
			return fmt.Errorf("lyrics database %s is not writable: %w\n"+
				"  this job's only effect is to write lyrics rows, so there is nothing for it to do.\n"+
				"  SQLite also writes -wal and -shm files next to it, so the job needs write\n"+
				"  access to both the file and its directory. Run the job as the owning user.",
				opts.LyricsDBPath, err)
		}
	}

	selection := selectionFrom(opts)
	pending, err := db.CountTracksMissingLyrics(ctx, opts.MetadataDB)
	if err != nil {
		return err
	}
	// The refresh set is only counted when it will be worked on, so the header
	// does not advertise a number of tracks the run will never touch.
	stale := int64(0)
	if selection.enabled {
		if stale, err = db.CountStaleLyrics(ctx, opts.MetadataDB, selection.cutoff()); err != nil {
			return err
		}
	}
	if pending+stale == 0 {
		fmt.Fprintln(opts.Out, "Nothing to do: every track already has an upstream lyrics answer.")
		if !selection.enabled {
			fmt.Fprintln(opts.Out, "  use -refresh to re-fetch tracks a provider already answered.")
		}
		return nil
	}

	planned := pending + stale
	if opts.Limit > 0 && int64(opts.Limit) < planned {
		planned = int64(opts.Limit)
	}

	workers := opts.Concurrency
	if workers < 1 {
		workers = 1
	}
	rate := opts.RatePerMinute

	// Built before the header so the slot ceiling can be reported against the real
	// provider count. A configuration with one provider enabled cannot use four
	// slots for anything: three of them would be queueing for the same upstream,
	// which is paced to one request at a time.
	resolver := newLyricsBackfillResolver(opts.Config, opts.MetadataDB, opts.UserIdleGap, opts.ErrOut)
	if resolver == nil {
		return errors.New("no lyrics providers could be configured; check LRCLIB_BASE_URL and the other lyrics provider settings")
	}
	slots := workers
	if providers := resolver.count(); providers > 0 && slots > providers {
		slots = providers
	}

	fmt.Fprintf(opts.Out, "Lyrics backfill\n")
	fmt.Fprintf(opts.Out, "  never settled            : %s\n", exactCount(pending))
	if selection.enabled {
		fmt.Fprintf(opts.Out, "  settled before cutoff    : %s\n", exactCount(stale))
	}
	fmt.Fprintf(opts.Out, "  songs in flight          : %d, spread across providers\n", slots)
	if rate > 0 {
		fmt.Fprintf(opts.Out, "  track ceiling            : %d/min\n", rate)
	} else {
		fmt.Fprintf(opts.Out, "  track ceiling            : provider pacing only\n")
	}
	fmt.Fprintf(opts.Out, "  per provider              : %s, every provider asked about every song\n", paceText(opts.Config.UpstreamLyricsJobInterval()))
	fmt.Fprintf(opts.Out, "  selection                : %s\n", selection.describe())
	fmt.Fprintf(opts.Out, "  scope                    : plain and synced lyrics, not word-level sync\n")
	if !selection.enabled {
		fmt.Fprintf(opts.Out, "  resume                   : a song a previous run left partway is asked only about the providers still missing\n")
	}
	if opts.DryRun {
		fmt.Fprintf(opts.Out, "  mode                     : dry run (no writes)\n")
	}
	fmt.Fprintf(opts.Out, "  providers                : %s\n\n", describeProviders(resolver))

	// Lanes are providers rather than slots, because the rate that matters is the
	// one each upstream is being asked at. Total is the song count, which is the
	// ceiling on how many songs any single provider can be asked about; a run that
	// resumes leaves a lane short of it, since the providers already answered are
	// not asked again.
	lanes := make([]*Lane, 0, resolver.count())
	for _, name := range resolver.names() {
		lanes = append(lanes, &Lane{Name: name, Total: planned})
	}
	progress := opts.Progress
	if progress == nil {
		progress = NewProgress(opts.Out, lanes)
	}
	stopProgress := progress.Start()
	defer stopProgress()

	// Announce the run so the live server can see that a job is active, and clear
	// the marker however the run ends.
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

	run := &lyricsBackfillRun{
		opts:           opts,
		resolver:       resolver,
		gate:           NewGate(rate),
		progress:       progress,
		lanes:          lanes,
		selection:      selection,
		slots:          slots,
		maxWriteErrors: opts.MaxWriteErrors,
		writeAbort:     make(chan struct{}),
		writeOK:        make(chan struct{}, 1),
		watchDone:      make(chan struct{}),
	}

	if err := run.execute(ctx, planned); err != nil {
		return err
	}

	rateLimits, pauses := run.gate.Stats()
	// A run that stopped because it could not write is not a clean finish, and must
	// not be reported as one: the operator needs to know the work is still
	// outstanding and why.
	if reason := run.abort(); reason != nil {
		fmt.Fprintf(opts.ErrOut, "\nStopped early. %v\n", reason)
		fmt.Fprintf(opts.ErrOut, "%s\n", run.summary())
		return reason
	}

	fmt.Fprintf(opts.Out, "\nDone. %s\n", run.summary())
	if rateLimits > 0 {
		fmt.Fprintf(opts.Out, "Upstream rate limits: %d absorbed, %d pauses.\n", rateLimits, pauses)
		if retried := run.deferredCount(); retried > 0 {
			fmt.Fprintf(opts.Out, "%s rate-limited lookup(s) were retried in this run.\n", humanCount(retried))
		}
	}
	if remaining := run.remaining(ctx); remaining > 0 {
		fmt.Fprintf(opts.Out, "%s track(s) still have no upstream lyrics answer from every provider; re-run this job to retry them, and it will ask only the providers still missing.\n", humanCount(remaining))
	}
	return nil
}

// lyricsBackfillRun holds the state shared by the producer, the slots, and the
// single writer goroutine. Anything belonging to one song lives on that song and
// is owned by one slot, so nothing here is a song's working state.
type lyricsBackfillRun struct {
	opts      Options
	resolver  *lyricsBackfillResolver
	gate      *Gate
	progress  *Progress
	lanes     []*Lane
	selection refreshSelection
	// slots is how many songs may be in flight at once. Each slot owns one song
	// end to end, which is what keeps per-song state off the shared struct: nothing
	// below has to be locked because only one goroutine ever touches a song.
	slots int

	writes chan lyricsWriteOp

	mu sync.Mutex
	// settled counts songs every provider gave a definitive answer to, which is the
	// unit the run actually promises to finish.
	settled int64
	// answers counts provider answers written, so the summary can show that a run
	// asked every provider rather than stopping at the first answer.
	answers       int64
	alreadyCached int64
	resumed       int64
	missed        int64
	throttled     int64
	failed        int64
	deferred      int64
	rejections    int64
	writeErrors   int64

	// consecutiveWriteErrors counts failed writes back to back. A run that cannot
	// persist anything must stop rather than keep spending upstream requests on
	// lookups whose results are discarded.
	consecutiveWriteErrors int
	maxWriteErrors         int
	writeAbort             chan struct{}
	abortOnce              sync.Once
	watchDone              chan struct{}
	abortReason            error
	writeOK                chan struct{}
}

func (r *lyricsBackfillRun) execute(ctx context.Context, planned int64) error {
	// runCtx is cancelled internally once the workers stop, so the caller's
	// context is what decides whether the run ended in error.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Write failures stop the run from inside the writer, so the workers have to
	// observe it. Deriving their context here means cancellation propagates to the
	// gate and the pacers as an ordinary context error, and the producer unwinds
	// through the same path as an operator interrupt.
	workCtx, stopWork := context.WithCancel(runCtx)
	defer stopWork()
	watcherDone := make(chan struct{})
	// Release the watcher on every exit path, not just an abort. It is a single
	// goroutine per run and this job is built to be callable in a loop, so leaking
	// one per successful run would accumulate.
	defer func() {
		close(r.watchDone)
		<-watcherDone
	}()
	r.watchWrites(stopWork, watcherDone)

	r.writes = make(chan lyricsWriteOp, lyricsWriteBatch*2)

	// One writer owns every mutation. Funneling writes through a single goroutine
	// keeps the commit path serialized and bounded, which is what makes it safe to
	// run alongside the live server.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		// The writer deliberately runs on the caller's context, not runCtx:
		// cancelling runCtx below only stops fetching, and the batch already
		// resolved still has to be committed.
		r.writeLoop(ctx)
	}()

	songs := make(chan *lyricsSong)
	var slots sync.WaitGroup
	for i := 0; i < r.slots; i++ {
		slots.Add(1)
		go func(index int) {
			defer slots.Done()
			r.slot(workCtx, index, songs)
		}(i)
	}

	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(songs)
		r.produce(workCtx, songs, planned)
	}()

	slots.Wait()
	// Unblock the producer, which may still be waiting to hand off a track.
	cancel()
	<-producerDone
	// Every slot has stopped, so nothing else can enqueue a write. Closing the
	// channel lets the writer flush its final batch and exit on its own terms
	// rather than being torn down mid-transaction.
	close(r.writes)
	<-writerDone
	return ctx.Err()
}

// lyricsSong is one track being worked through the providers it still needs.
//
// A slot owns its song from the moment it takes it to the moment it settles it, so
// everything accumulated about a song stays local to one goroutine. That is why
// none of this is shared state: there is nothing to lock, and two slots cannot be
// looking at the same song.
type lyricsSong struct {
	work db.LyricsWork
	// pending lists the provider indices still to be asked, in the order this
	// song's slot will take them.
	pending []int
	// attempted counts the tries each provider has already had, so a throttle can
	// be retried a bounded number of times per provider rather than per song.
	attempted map[int]int
	// cached marks a song whose lyrics are already on disk, which is settled
	// without asking anybody.
	cached bool
	// resumed marks a song a previous run left partway, so the run can say how much
	// of the work was already done rather than claiming all of it.
	resumed bool
	// unanswered marks a song where a provider gave up before answering. It is the
	// one thing that stops the song being settled, because a track is only answered
	// when every provider has spoken.
	unanswered bool
}

// produce streams pages so a library of any size runs in constant memory.
//
// It walks the same two phases as the metadata backfill: never-settled work
// first, then the refresh set, with the first phase drained completely before the
// second begins. That ordering is what makes a limited run spend its budget on
// tracks that have never been asked rather than re-confirming ones that already
// have lyrics, and it holds however the two sets interleave in the table.
func (r *lyricsBackfillRun) produce(ctx context.Context, out chan<- *lyricsSong, planned int64) {
	produced := r.producePending(ctx, out, planned)
	if produced >= planned || !r.selection.enabled {
		return
	}
	r.produceStale(ctx, out, planned-produced)
}

// producePending feeds tracks no lyrics provider has ever settled.
func (r *lyricsBackfillRun) producePending(ctx context.Context, out chan<- *lyricsSong, budget int64) int64 {
	if budget <= 0 {
		return 0
	}
	var afterID int64
	var produced int64
	for produced < budget {
		if ctx.Err() != nil {
			return produced
		}
		page, err := db.ListTracksMissingLyrics(ctx, r.opts.MetadataDB, afterID, pageSize)
		if err != nil {
			if ctx.Err() == nil {
				r.progress.Notice("read tracks: %v", err)
			}
			return produced
		}
		if len(page) == 0 {
			return produced
		}
		// The cursor moves past the whole page even when the page turned out to be
		// entirely cached or already answered: those tracks need no work, and
		// re-reading them would make the scan loop without making progress.
		afterID = page[len(page)-1].ID
		produced += r.emitPage(ctx, out, page, budget-produced)
	}
	return produced
}

// produceStale feeds tracks a lyrics provider settled before the refresh cutoff.
//
// It pages on (age, id) because it orders by age. A row the run settles gets a
// fresh settle time and leaves the set; a row it could not settle keeps its old
// one but is still not re-read, because the cursor only advances past it.
func (r *lyricsBackfillRun) produceStale(ctx context.Context, out chan<- *lyricsSong, budget int64) int64 {
	if budget <= 0 {
		return 0
	}
	var afterAge string
	var afterID int64
	var produced int64
	for produced < budget {
		if ctx.Err() != nil {
			return produced
		}
		page, err := db.ListStaleLyrics(ctx, r.opts.MetadataDB, r.selection.cutoff(), afterAge, afterID, pageSize)
		if err != nil {
			if ctx.Err() == nil {
				r.progress.Notice("read stale tracks: %v", err)
			}
			return produced
		}
		if len(page) == 0 {
			return produced
		}
		last := page[len(page)-1]
		afterAge, afterID = last.CheckedAt, last.ID
		produced += r.emitPage(ctx, out, page, budget-produced)
	}
	return produced
}

// emitPage turns one page of tracks into songs and hands them to the slots.
//
// The two per-page reads happen here rather than in a slot because both are
// questions about a whole page: which songs already have lyrics worth serving,
// and which providers a previous run has already asked. Asking per song instead
// would turn a 500-track page into a thousand queries against a database the live
// server is also writing.
func (r *lyricsBackfillRun) emitPage(ctx context.Context, out chan<- *lyricsSong, page []db.LyricsWork, budget int64) int64 {
	if budget <= 0 || len(page) == 0 {
		return 0
	}
	ids := make([]int64, 0, len(page))
	for _, item := range page {
		ids = append(ids, item.ID)
	}
	// A track that already has lyrics cached costs nothing, and asking a provider
	// to confirm what is stored would spend the run's upstream budget on nothing.
	//
	// A read failure here is not fatal: the worst case is that one track is asked
	// upstream for lyrics it already has, which is exactly what a run without this
	// check would do anyway.
	var cached map[int64]bool
	if !r.selection.enabled {
		var err error
		cached, err = db.TrackIDsWithUsableLyrics(ctx, r.opts.LyricsDB, ids)
		if err != nil {
			cached = nil
		}
	}
	answered := r.alreadyAnswered(ctx, ids)
	names := r.resolver.names()

	var produced int64
	for _, item := range page {
		if produced >= budget {
			break
		}
		song := &lyricsSong{work: item, attempted: make(map[int]int)}
		if cached[item.ID] {
			song.cached = true
		} else {
			asked := answered[item.ID]
			for index, name := range names {
				// Presence, not the value. The ledger records whether the last
				// attempt succeeded, but either way it records that the provider was
				// asked, and a provider that has already said no should not be asked
				// again on the next run.
				if _, covered := asked[name]; !covered {
					song.pending = append(song.pending, index)
				}
			}
			song.resumed = len(song.pending) < len(names)
			if song.resumed {
				r.noteResumed()
			}
		}
		select {
		case <-ctx.Done():
			return produced
		case out <- song:
		}
		produced++
	}
	return produced
}

// alreadyAnswered reports which providers each track in a page has already been
// asked about.
//
// It returns nothing when the set is being refreshed, because a refresh is the
// operator asking again on purpose and the ledger is exactly what that has to
// ignore. It also returns nothing when the read fails, and that direction is
// deliberate: re-asking a provider that had answered costs one request, whereas
// treating a provider as answered that never was would leave the song unsettled
// forever with nothing left to tell the next run what it still owed.
func (r *lyricsBackfillRun) alreadyAnswered(ctx context.Context, ids []int64) map[int64]map[string]bool {
	if r.selection.enabled {
		return nil
	}
	fetches, err := db.ListProviderFetchesForTracks(ctx, r.opts.LyricsDB, ids)
	if err != nil {
		if ctx.Err() == nil {
			r.progress.Notice("read provider fetches: %v", err)
		}
		return nil
	}
	return fetches
}

// slot works one song at a time, from every provider it needs to settled.
//
// Each slot is given a stride into the provider order rather than starting every
// song at the first provider. That is the whole point of walking the providers in
// turn: every provider serves one request per interval, so two songs on the same
// provider queue behind each other while two on different ones make progress
// together. Rotating by the slot's index puts the slots one provider apart, which
// is the most even spread a fixed set of slots can manage.
func (r *lyricsBackfillRun) slot(ctx context.Context, index int, songs <-chan *lyricsSong) {
	for {
		select {
		case <-ctx.Done():
			return
		case song, ok := <-songs:
			if !ok {
				return
			}
			r.runSong(ctx, song, index)
			// A run that cannot persist anything must not keep spending upstream
			// requests. Cancelling the work context stops every other slot too, so
			// the remaining songs are left for the next run, which is where they
			// would have ended up regardless.
			if r.writeFailed() {
				return
			}
		}
	}
}

// runSong works one song through every provider it still needs and settles it
// once the last one has answered, reporting whether the slot should keep going.
//
// Each answer is written the moment its provider gives it rather than held to the
// end, so an interrupted run keeps what it got and the next one picks the song up
// from there. What the answers are used for is decided in two steps: the row goes
// down immediately so the request path has something to serve, and the settle at
// the end decides which of the stored rows the track actually points at.
func (r *lyricsBackfillRun) runSong(ctx context.Context, song *lyricsSong, stride int) bool {
	if song.cached {
		r.enqueue(ctx, lyricsWriteOp{work: song.work, settleUnknownAge: true})
		return !r.writeFailed()
	}

	if err := r.gate.Wait(ctx); err != nil {
		return false
	}

	providers := r.resolver.count()
	song.pending = rotatePending(song.pending, stride, providers)
	for len(song.pending) > 0 {
		index := song.pending[0]
		song.pending = song.pending[1:]
		if !r.askProvider(ctx, song, index) {
			return false
		}
		if r.writeFailed() {
			return false
		}
	}

	// A provider that ran out of retries stops re-queueing itself, which empties the
	// pending list without the song having been answered by everyone. Settling here
	// would record "checked" on the strength of the providers that did reply, which
	// is exactly the permanent negative a bad minute must not leave behind. The song
	// stays unsettled and the ledger says which provider still owes an answer, so
	// the next run picks up from that pair alone.
	if song.unanswered {
		return !r.writeFailed()
	}

	// Every provider has now given a definitive answer, so the song can be recorded
	// as settled. A song with nothing stored settles too: "no provider has these
	// lyrics" is an answer, and without recording it the run would ask the same
	// question of every provider on every pass forever.
	r.enqueue(ctx, lyricsWriteOp{work: song.work, settle: true})
	return !r.writeFailed()
}

// rotatePending reorders a song's pending providers so this slot starts on the
// provider given by stride.
//
// The rotation is applied to the list rather than to an index into it, so a
// resumed song that only has two providers left still starts on a different one
// per slot rather than collapsing back onto whichever providers remain.
func rotatePending(pending []int, stride, providers int) []int {
	if providers <= 1 || len(pending) <= 1 {
		return pending
	}
	offset := stride % len(pending)
	if offset == 0 {
		return pending
	}
	rotated := make([]int, 0, len(pending))
	rotated = append(rotated, pending[offset:]...)
	rotated = append(rotated, pending[:offset]...)
	return rotated
}

// askProvider queries one provider about the song and files its answer, reporting
// whether the slot can carry on.
//
// A provider that answers definitively is done with. One that is rate limited goes
// back on the song's pending list for a bounded number of further attempts, so a
// bad minute for one upstream costs a retry of that one provider rather than a
// fresh attempt at all of them and an abandonment of the answers already
// collected. Anything else is left alone: a refusal or a timeout is more likely
// about this one request than about the song, and retrying every failure would let
// one broken provider stall the whole run.
func (r *lyricsBackfillRun) askProvider(ctx context.Context, song *lyricsSong, index int) bool {
	// A rate limit seen anywhere holds the whole run, so the rest of this song does
	// not go out during the cooldown the provider just asked for.
	if err := r.gate.WaitThrottle(ctx); err != nil {
		return false
	}
	names := r.resolver.names()
	if index < 0 || index >= len(names) {
		return true
	}
	name := names[index]
	label := songLabel(song.work)
	result := r.resolver.Lookup(ctx, index, song.work)
	r.progress.Advance(r.lanes[index], result.outcome, label)

	switch result.outcome {
	case OutcomeSucceeded:
		r.gate.Succeed()
		r.enqueue(ctx, lyricsWriteOp{work: song.work, answer: &lyricsHit{provider: name, lyrics: result.lyrics}})
	case OutcomeMissing:
		r.gate.Succeed()
		// A definitive miss still has to reach the ledger, or the request path
		// would ask the same provider about the same song again.
		r.enqueue(ctx, lyricsWriteOp{work: song.work, missed: name, recordMiss: completeIdentity(song.work)})
		r.noteMissed()
	case OutcomeThrottled:
		r.gate.Throttle(result.retryAfter)
		r.noteRejected(ctx, result)
		r.countOutcome(result.outcome)
		if ctx.Err() == nil && result.err != nil {
			r.progress.Notice("%s lookup %q: %v", name, label, result.err)
		}
		if song.attempted[index] < maxThrottleRetries {
			song.attempted[index]++
			song.pending = append(song.pending, index)
			r.noteDeferred()
			break
		}
		// Out of retries. The song is left unsettled, which is the honest record:
		// nothing was established about whether it has lyrics, and the ledger still
		// says which provider owes an answer for the next run.
		song.unanswered = true
	default:
		r.noteRejected(ctx, result)
		r.countOutcome(result.outcome)
		if ctx.Err() == nil && result.err != nil {
			r.progress.Notice("%s lookup %q: %v", name, label, result.err)
		}
		song.unanswered = true
	}
	return true
}

// songLabel renders a track for the progress display.
func songLabel(work db.LyricsWork) string {
	if work.Artist != "" {
		return work.Name + " - " + work.Artist
	}
	return work.Name
}

// noteRejected counts a provider that refused this host and pauses the slot.
//
// A refusal is a property of the caller rather than of any one song, so the other
// providers may still work and are not abandoned. The pause keeps this slot from
// carrying on into the same wall, and the notice names the provider so an operator
// can see which upstream is the problem.
func (r *lyricsBackfillRun) noteRejected(ctx context.Context, result providerLookup) {
	if !result.rejected {
		return
	}
	r.mu.Lock()
	r.rejections++
	r.mu.Unlock()
	r.progress.Notice("provider refused (%s); pausing %s", result.rejection, lyricsRejectionPause)
	select {
	case <-ctx.Done():
	case <-time.After(lyricsRejectionPause):
	}
}

// countOutcome tallies the outcomes that are not stored anywhere, because there is
// nothing to store. An answer or a definitive miss is counted from the committed
// batch instead, so the summary reports what was actually persisted rather than
// what was merely looked up: a run interrupted mid-flight drops queued writes, and
// those answers are looked up again on the next pass.
func (r *lyricsBackfillRun) countOutcome(outcome Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch outcome {
	case OutcomeThrottled:
		r.throttled++
	case OutcomeFailed:
		r.failed++
	}
}

// noteResumed records a song a previous run had left partway.
func (r *lyricsBackfillRun) noteResumed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumed++
}

// noteMissed records a definitive negative from one provider.
func (r *lyricsBackfillRun) noteMissed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.missed++
}

// noteDeferred records a rate-limited lookup that is being tried again in this
// run, as opposed to one written off for the next run.
func (r *lyricsBackfillRun) noteDeferred() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferred++
}

// countCommitted tallies a batch that was durably written.
func (r *lyricsBackfillRun) countCommitted(batch []lyricsWriteOp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, op := range batch {
		switch {
		case op.settleUnknownAge:
			r.alreadyCached++
		case op.settle:
			r.settled++
		case op.answer != nil:
			r.answers++
		}
	}
}

func (r *lyricsBackfillRun) enqueue(ctx context.Context, op lyricsWriteOp) {
	select {
	case r.writes <- op:
	case <-ctx.Done():
	}
}

// writeLoop commits pending operations in batches. A batch is flushed when it
// fills, when the flush interval elapses, or when the run ends, so a partially
// filled batch is never left uncommitted.
func (r *lyricsBackfillRun) writeLoop(ctx context.Context) {
	batch := make([]lyricsWriteOp, 0, lyricsWriteBatch)
	ticker := time.NewTicker(lyricsBatchFlushInterval)
	defer ticker.Stop()

	flush := func(flushCtx context.Context) {
		if len(batch) == 0 {
			return
		}
		r.commit(flushCtx, batch)
		batch = batch[:0]
		// Keep the frame's failure count current even between commits, so an abort
		// in progress is visible rather than only in the final summary.
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
			if len(batch) >= lyricsWriteBatch {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-ctx.Done():
			// The caller is interrupting. Commit what is already resolved using a
			// detached context: the run context is cancelled, so reusing it here
			// would fail to open a transaction and silently discard the final
			// partial batch. A short bound keeps shutdown from hanging on a
			// database lock held by the live server.
			detach, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			flush(detach)
			cancel()
			return
		}
	}
}

// commit applies one batch, stopping at the first operation that fails.
//
// A batch cannot be atomic across the two databases, because SQLite has no
// two-database transaction. The order each operation uses makes that safe: the
// lyrics row is written before the metadata flag, so a crash in between leaves a
// track with lyrics but no flag, which the next run re-settles. The reverse order
// would leave a track recorded as answered that has no lyrics, which nothing
// would ever revisit.
func (r *lyricsBackfillRun) commit(ctx context.Context, batch []lyricsWriteOp) {
	if r.opts.DryRun {
		return
	}
	for _, op := range batch {
		if err := r.applyOp(ctx, op); err != nil {
			r.noteWriteError(err)
			r.reportWriteError("commit batch", err)
			return
		}
	}
	r.countCommitted(batch)
	r.noteWriteSuccess()
}

// applyOp commits one operation across both databases.
//
// The three shapes are deliberately not merged into one transaction per song. An
// answer is written the moment it arrives so an interrupted run keeps it, and the
// settle that follows is the only step allowed to claim the song is answered.
func (r *lyricsBackfillRun) applyOp(ctx context.Context, op lyricsWriteOp) error {
	switch {
	case op.answer != nil:
		return r.applyAnswer(ctx, op)
	case op.settle:
		if err := db.SettleTrackLyrics(ctx, r.opts.MetadataDB, r.opts.LyricsDB, op.work.ID); err != nil {
			return fmt.Errorf("settle track %d: %w", op.work.ID, err)
		}
		return nil
	case op.missed != "":
		// A definitive negative is worth remembering on its own, whether or not the
		// song ends up settled: it is what stops the request path asking the same
		// provider about a song it has already been told nothing for.
		//
		// A miss is only recorded for a complete identity. The ledger is keyed by
		// track alone, so a miss stored under a partial one would wrongly suppress
		// a later retry made once the artist or album is known.
		if op.recordMiss {
			r.recordProviderFetch(ctx, op.work.ID, op.missed, false)
		}
		return nil
	}

	// The track already had usable lyrics cached, so it is stamped settled without
	// anything having been asked upstream. The settle time is deliberately left
	// empty: see MarkTrackLyricsSettledUnknownAge.
	tx, err := r.opts.MetadataDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin lyrics settle: %w", err)
	}
	if err := db.MarkTrackLyricsSettledUnknownAge(ctx, tx, op.work.ID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit lyrics settle: %w", err)
	}
	return nil
}

// applyAnswer stores one provider's answer and files it in the ledger.
//
// The served pointer is moved as well, rather than waiting for the song's settle.
// The request path finds a track's lyrics only through last_lyrics_id, so an
// answer stored without it is invisible to the server until the whole song is done,
// which under a slower per-song walk is most of the run. Pointing at it here means
// a song is playable as soon as any one provider has answered it, and the settle
// still gets to replace that answer with a better one later.
//
// The pointer only moves to a strict improvement, so an answer that is worse than
// what the track already serves is stored and remembered without displacing it.
func (r *lyricsBackfillRun) applyAnswer(ctx context.Context, op lyricsWriteOp) error {
	hit := op.answer
	answer := *hit.lyrics
	// Every provider's row carries its own name in source, so an alternative that
	// lost on quality is still identifiable on disk.
	answer.Source = hit.provider

	lyricsID, err := db.StoreLyricsAnswer(ctx, r.opts.LyricsDB, op.work.ID, answer)
	if err != nil {
		return fmt.Errorf("store lyrics for track %d: %w", op.work.ID, err)
	}
	if _, err := db.PointTrackAtBestLyrics(ctx, r.opts.MetadataDB, r.opts.LyricsDB, op.work.ID, []int64{lyricsID}); err != nil {
		return fmt.Errorf("link lyrics for track %d: %w", op.work.ID, err)
	}
	// The ledger is told about this provider, not just the winner. It tells the
	// live path which upstreams it need not spend again, and a provider whose
	// answer is stored but lost the pointer is still one it should not ask.
	r.recordProviderFetch(ctx, op.work.ID, hit.provider, true)
	return nil
}

// recordProviderFetch notes an attempt in the per-provider fetch table.
//
// It is fire-and-forget: the fetch ledger is an optimization that saves the live
// path an upstream call, so failing to record one must not fail the run. The track
// flag written alongside it is the part that matters.
func (r *lyricsBackfillRun) recordProviderFetch(ctx context.Context, trackID int64, provider string, success bool) {
	if strings.TrimSpace(provider) == "" || trackID <= 0 {
		return
	}
	_ = db.UpsertProviderFetch(ctx, r.opts.LyricsDB, trackID, provider, success)
}

// trackForLyricsWork rebuilds the metadata row for a fetched result.
//
// The row is addressed by its existing id, and the lowercased identity is carried
// across verbatim so the upsert's conflict target matches the row already in
// place. Sending blank lowercased values would let it try to fill them in, which
// on a row that already has them is a no-op but on a partial one would re-key the
// track.
func trackForLyricsWork(work db.LyricsWork) db.Track {
	return db.Track{
		ID:              work.ID,
		Name:            work.Name,
		NameLower:       work.Name,
		ArtistName:      work.Artist,
		ArtistNameLower: work.Artist,
		AlbumName:       work.Album,
		AlbumNameLower:  work.Album,
		Duration:        work.Duration,
		ISRC:            work.ISRC,
		Source:          "lyrics_backfill",
	}
}

// completeIdentity reports whether a track has both an artist and an album, which
// is what a lyrics miss may safely be recorded under.
func completeIdentity(work db.LyricsWork) bool {
	return strings.TrimSpace(work.Artist) != "" && strings.TrimSpace(work.Album) != ""
}

func (r *lyricsBackfillRun) noteWriteError(err error) {
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
	// Stop the workers while the writer is still on the caller's context, so the run
	// unwinds through the normal path and the final batch is flushed.
	r.abortOnce.Do(func() { close(r.writeAbort) })
}

// writeErrorCount reports the total number of failed write batches.
func (r *lyricsBackfillRun) writeErrorCount() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeErrors
}

// writeFailed reports whether the writer has given up, so a worker can stop early
// instead of running a lookups-only pass to the end of the queue.
func (r *lyricsBackfillRun) writeFailed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abortReason != nil
}

// abort returns the reason the run stopped early, or nil.
func (r *lyricsBackfillRun) abort() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abortReason
}

// reportWriteError records a write failure for the frame and prints it. The
// message goes through the renderer so it cannot be swallowed by an in-place
// redraw that is happening at the same moment.
func (r *lyricsBackfillRun) reportWriteError(stage string, err error) {
	r.progress.Notice("%s: %v", stage, err)
	if reason := r.abort(); reason != nil {
		r.progress.Notice("%v", reason)
	}
}

// noteWriteSuccess resets the consecutive failure streak, so a run that hits a busy
// database a few times is not aborted by failures that are no longer happening.
func (r *lyricsBackfillRun) noteWriteSuccess() {
	r.mu.Lock()
	had := r.consecutiveWriteErrors > 0
	r.consecutiveWriteErrors = 0
	r.mu.Unlock()
	if !had {
		return
	}
	// Signal without blocking: the watcher is a single goroutine and a pending reset
	// is equivalent to a delivered one.
	select {
	case r.writeOK <- struct{}{}:
	default:
	}
}

// watchWrites cancels the workers' context once the writer decides the run cannot
// continue, and is released on every exit path so a successful run does not leak
// the goroutine.
func (r *lyricsBackfillRun) watchWrites(stopWork context.CancelFunc, done chan<- struct{}) {
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
				// A successful write already cleared the streak; nothing to do
				// beyond keeping the signal drained.
			}
		}
	}()
}

// deferredCount reports how many rate-limited lookups were retried in this run
// rather than written off for the next one.
func (r *lyricsBackfillRun) deferredCount() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deferred
}

// remaining reports how many tracks are still never-settled, so the summary can say
// whether a plain re-run would do anything. The stale set is deliberately left
// out: a refresh run leaves freshly settled rows behind on purpose, and reporting
// them as outstanding would tell the operator to immediately repeat the refresh.
func (r *lyricsBackfillRun) remaining(ctx context.Context) int64 {
	if r.opts.DryRun {
		return 0
	}
	total, err := db.CountTracksMissingLyrics(ctx, r.opts.MetadataDB)
	if err != nil {
		return 0
	}
	return total
}

func (r *lyricsBackfillRun) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The song count comes first because it is the unit the run promises to
	// finish: a song is settled only when every provider has answered it. The
	// lookup counts follow, because a run that settled few songs may still have
	// written many answers, and that is the difference between a run that was
	// thorough and one that was cut short.
	parts := []string{
		fmt.Sprintf("%s tracks settled", exactCount(r.settled)),
		fmt.Sprintf("%s lyrics fetched", exactCount(r.answers)),
		fmt.Sprintf("%s not found upstream", exactCount(r.missed)),
	}
	if r.alreadyCached > 0 {
		parts = append(parts, fmt.Sprintf("%s already cached locally", exactCount(r.alreadyCached)))
	}
	if r.resumed > 0 {
		parts = append(parts, fmt.Sprintf("%s resumed from a partial run", exactCount(r.resumed)))
	}
	if r.throttled > 0 {
		parts = append(parts, fmt.Sprintf("%s rate limited", exactCount(r.throttled)))
	}
	if r.deferred > 0 {
		parts = append(parts, fmt.Sprintf("%s rate limited and retried", exactCount(r.deferred)))
	}
	if r.failed > 0 {
		parts = append(parts, fmt.Sprintf("%s failed", exactCount(r.failed)))
	}
	if r.rejections > 0 {
		parts = append(parts, fmt.Sprintf("%s rejected by a provider", exactCount(r.rejections)))
	}
	if r.writeErrors > 0 {
		parts = append(parts, fmt.Sprintf("%s write errors", exactCount(r.writeErrors)))
	}
	return strings.Join(parts, ", ") + "."
}
