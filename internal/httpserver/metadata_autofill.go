package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/config"
	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
)

const (
	// metadataAutofillIdleGap is how long live traffic must stay quiet before
	// the routine sweep resumes. A request inside this window pauses the sweep,
	// and the gap starts again from the next request, so a server in steady use
	// never spends upstream budget behind its users' backs.
	metadataAutofillIdleGap = time.Minute

	// metadataAutofillPriorityTTL is how long a track a client actually asked
	// for keeps its place at the front of the queue. Inside this window a repeat
	// request is answered from the settled row; after it the track falls back
	// into the ordinary pending set, where nothing is lost.
	metadataAutofillPriorityTTL = 10 * time.Minute

	// metadataAutofillTickInterval is how often a sleeping worker looks for
	// work, and how long it waits after a pause or an empty pass before
	// checking again.
	metadataAutofillTickInterval = 15 * time.Second

	// metadataAutofillStartupDelay lets the process settle before it starts
	// spending an upstream budget on the library.
	metadataAutofillStartupDelay = 30 * time.Second

	// metadataAutofillPageSize is how many pending tracks are read at a time.
	// The page is drained one track per lookup, so a page lasts a while and a
	// large library is never held in memory.
	metadataAutofillPageSize = 50

	// metadataAutofillPassPause is how long a completed sweep waits before it
	// starts over. Without it an empty library would be re-read continuously.
	metadataAutofillPassPause = time.Minute

	// metadataAutofillMaxPriority bounds the queue of recently requested
	// tracks. A burst of misses must cost bounded memory, so past this size the
	// oldest request is dropped; it is still in the pending set the sweep walks.
	metadataAutofillMaxPriority = 256

	// metadataAutofillAttempts is how many extra attempts a requested track
	// gets when every provider answered inconclusively (throttled, refused, or
	// unreachable). A throttle describes the moment, not the song, so one retry
	// is worth it; more than that is hammering an upstream that has already
	// said no.
	metadataAutofillAttempts = 2

	// metadataAutofillLookupTimeout bounds one upstream lookup, and
	// metadataAutofillWriteTimeout bounds the write that persists it. The write
	// is detached from the lookup's deadline: a slow provider must not make a
	// resolved answer fail to land.
	metadataAutofillLookupTimeout = 30 * time.Second
	metadataAutofillWriteTimeout  = 10 * time.Second
)

// metadataAutofillItem is one track waiting to be resolved against a metadata
// provider, either because a client asked for it or because the routine sweep
// reached it.
type metadataAutofillItem struct {
	trackID   int64
	name      string
	artist    string
	album     string
	duration  float64
	requested time.Time
	// attempts counts the retries already granted to a requested track.
	attempts int
}

// metadataAutofill keeps the metadata cache complete without anyone running
// `music-utils --run-job metadata-backfill`.
//
// Tracks that entered the library through a lyrics lookup carry no genre, ISRC,
// or MusicBrainz ID and nothing in the request path ever goes back for them.
// This walks that set whenever the instance is idle, so the cache closes its
// own gaps instead of waiting for an operator to remember a command.
//
// It has two priorities rather than one queue. A track a client just asked for
// is retried immediately, even while the server is busy, because that client is
// waiting on this answer and a miss a minute ago may simply have been an
// upstream hiccup. Everything else waits for the idle window, which is what
// keeps the sweep from competing with real traffic.
//
// It shares the providers (and therefore the shared upstream pacing) of the
// request path, so the automatic sweep can never out-spend the live server.
type metadataAutofill struct {
	database *sql.DB
	resolver *metadata.Resolver
	logger   *slog.Logger
	// idleGap and priorityTTL are the two windows above, overridable so tests do
	// not have to wait out a real minute.
	idleGap     time.Duration
	priorityTTL time.Duration

	// mu guards the fields below, which are touched by request goroutines and
	// by the worker.
	mu          sync.Mutex
	lastRequest time.Time
	priority    map[string]metadataAutofillItem

	// The sweep state is only ever touched by the worker goroutine.
	sweepCursor     int64
	sweepQueue      []db.MetadataWork
	sweepPauseUntil time.Time

	wake     chan struct{}
	cancel   context.CancelFunc
	stopped  chan struct{}
	stopOnce sync.Once
}

// newMetadataAutofill starts the automatic backfill, or returns nil when there
// is nothing for it to do: no metadata database, no provider chain, or metadata
// fallback turned off entirely.
func newMetadataAutofill(cfg config.Config, database *sql.DB, resolver *metadata.Resolver, logger *slog.Logger) *metadataAutofill {
	if !cfg.MetadataFallbackEnabled || database == nil || resolver == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &metadataAutofill{
		database:    database,
		resolver:    resolver,
		logger:      logger,
		idleGap:     metadataAutofillIdleGap,
		priorityTTL: metadataAutofillPriorityTTL,
		priority:    make(map[string]metadataAutofillItem),
		wake:        make(chan struct{}, 1),
		cancel:      cancel,
		stopped:     make(chan struct{}),
	}
	go a.run(ctx)
	return a
}

// Stop halts the worker and waits for it to finish.
func (a *metadataAutofill) Stop() {
	if a == nil {
		return
	}
	a.stopOnce.Do(func() {
		a.cancel()
		<-a.stopped
	})
}

// trackActivity records every request that reaches the API, which is what
// pauses the routine sweep. Uptime probes, the version endpoint, and the stats
// endpoints are excluded on purpose: they are polled on a timer by things that
// are not users, and counting them would keep the sweep off forever.
func (a *metadataAutofill) trackActivity(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions && !autofillIgnoresActivity(r.URL.Path) {
			a.noteRequest()
		}
		next.ServeHTTP(w, r)
	})
}

func autofillIgnoresActivity(path string) bool {
	switch path {
	case "/api/healthz", "/api/version":
		return true
	}
	return strings.HasPrefix(path, "/api/stats/")
}

// noteRequest stamps live activity, which stops the sweep until the idle window
// has passed again.
func (a *metadataAutofill) noteRequest() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.lastRequest = time.Now()
	a.mu.Unlock()
}

// enqueue gives a track a client just failed to get from cache the highest
// priority, and wakes the worker so the retry starts now rather than at the next
// tick. A repeat request for the same track moves its window forward instead of
// queueing it twice.
func (a *metadataAutofill) enqueue(trackID int64, input metadata.Input) {
	if a == nil {
		return
	}
	name := strings.TrimSpace(input.TrackName)
	if name == "" {
		return
	}
	item := metadataAutofillItem{
		trackID:   trackID,
		name:      name,
		artist:    strings.TrimSpace(input.ArtistName),
		album:     strings.TrimSpace(input.AlbumName),
		duration:  input.Duration,
		requested: time.Now(),
	}
	key := autofillKey(item)

	a.mu.Lock()
	if a.priority == nil {
		a.priority = make(map[string]metadataAutofillItem)
	}
	if _, exists := a.priority[key]; !exists && len(a.priority) >= metadataAutofillMaxPriority {
		a.evictOldestLocked()
	}
	// A track already waiting keeps the attempts it has already spent, so a
	// client repeating a request cannot reset the retry budget.
	if queued, exists := a.priority[key]; exists {
		item.attempts = queued.attempts
	}
	a.priority[key] = item
	a.mu.Unlock()

	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// requeue puts an item back without starting its priority window over, so a
// track is not kept alive by its own retries.
func (a *metadataAutofill) requeue(item metadataAutofillItem) {
	a.mu.Lock()
	if a.priority == nil {
		a.priority = make(map[string]metadataAutofillItem)
	}
	if len(a.priority) >= metadataAutofillMaxPriority {
		if _, exists := a.priority[autofillKey(item)]; !exists {
			a.evictOldestLocked()
		}
	}
	a.priority[autofillKey(item)] = item
	a.mu.Unlock()

	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// evictOldestLocked drops the least recently requested entry. The caller holds
// mu.
func (a *metadataAutofill) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for key, item := range a.priority {
		if oldestKey == "" || item.requested.Before(oldest) {
			oldestKey, oldest = key, item.requested
		}
	}
	if oldestKey != "" {
		delete(a.priority, oldestKey)
	}
}

func autofillKey(item metadataAutofillItem) string {
	if item.trackID > 0 {
		return "id:" + strconv.FormatInt(item.trackID, 10)
	}
	return "q:" + strings.ToLower(item.name) + "\x00" + strings.ToLower(item.artist) + "\x00" + strings.ToLower(item.album)
}

// nextPriority removes and returns the most recently requested track still
// inside its priority window.
func (a *metadataAutofill) nextPriority(now time.Time) (metadataAutofillItem, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var best metadataAutofillItem
	var bestKey string
	for key, item := range a.priority {
		if now.Sub(item.requested) > a.priorityWindow() {
			delete(a.priority, key)
			continue
		}
		if bestKey == "" || item.requested.After(best.requested) {
			best, bestKey = item, key
		}
	}
	if bestKey == "" {
		return metadataAutofillItem{}, false
	}
	delete(a.priority, bestKey)
	return best, true
}

// idle reports whether the instance has been quiet long enough for routine
// backfilling.
func (a *metadataAutofill) idle(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastRequest.IsZero() {
		return true
	}
	return now.Sub(a.lastRequest) >= a.idleWindow()
}

func (a *metadataAutofill) idleWindow() time.Duration {
	if a.idleGap > 0 {
		return a.idleGap
	}
	return metadataAutofillIdleGap
}

func (a *metadataAutofill) priorityWindow() time.Duration {
	if a.priorityTTL > 0 {
		return a.priorityTTL
	}
	return metadataAutofillPriorityTTL
}

// run wakes on a timer or when a request hands it work, and drains whatever it
// is allowed to do before sleeping again.
func (a *metadataAutofill) run(ctx context.Context) {
	defer close(a.stopped)
	timer := time.NewTimer(metadataAutofillStartupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-timer.C:
		}
		a.work(ctx)
		resetAutofillTimer(timer, metadataAutofillTickInterval)
	}
}

// resetAutofillTimer restarts timer at d, discarding a pending tick.
func resetAutofillTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

// work drains the priority queue first and then the routine sweep, stopping as
// soon as a request arrives or there is nothing left to do.
func (a *metadataAutofill) work(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		// A track a client asked for outranks both the routine sweep and the
		// idle window: that caller is waiting on this answer.
		if item, ok := a.nextPriority(time.Now()); ok {
			a.backfill(ctx, item)
			continue
		}
		if !a.idle(time.Now()) {
			return
		}
		if !a.sweepOne(ctx) {
			return
		}
	}
}

// sweepOne resolves one pending track, reporting whether there was any work.
func (a *metadataAutofill) sweepOne(ctx context.Context) bool {
	if len(a.sweepQueue) > 0 {
		item := a.sweepQueue[0]
		a.sweepQueue = a.sweepQueue[1:]
		a.backfill(ctx, metadataAutofillItem{
			trackID:  item.ID,
			name:     item.Name,
			artist:   item.Artist,
			album:    item.Album,
			duration: item.Duration,
		})
		return true
	}
	if time.Now().Before(a.sweepPauseUntil) {
		return false
	}
	page, err := db.ListTracksMissingMetadata(ctx, a.database, a.sweepCursor, metadataAutofillPageSize)
	if err != nil {
		if ctx.Err() == nil {
			a.logger.Warn("automatic metadata backfill could not read pending tracks", "error", err)
		}
		a.sweepPauseUntil = time.Now().Add(metadataAutofillPassPause)
		return false
	}
	if len(page) == 0 {
		// The whole library has been walked. Start over after a pause so the
		// sweep does not spin on an empty set.
		a.sweepCursor = 0
		a.sweepPauseUntil = time.Now().Add(metadataAutofillPassPause)
		return false
	}
	// Advance the cursor past this page: a track that could not be settled stays
	// unchecked but is not re-read by this pass, which is what keeps the sweep
	// moving forward through a library with a few unresolvable songs in it.
	a.sweepCursor = page[len(page)-1].ID
	a.sweepQueue = append(a.sweepQueue, page[1:]...)
	item := page[0]
	a.backfill(ctx, metadataAutofillItem{
		trackID:  item.ID,
		name:     item.Name,
		artist:   item.Artist,
		album:    item.Album,
		duration: item.Duration,
	})
	return true
}

// backfill looks one track up and records the outcome.
//
// Only a definitive answer is written. A throttle or a provider failure leaves
// the row untouched so a later pass retries it, whereas an all-providers-agree
// miss is recorded, because that is what stops the sweep asking about the same
// song forever.
func (a *metadataAutofill) backfill(ctx context.Context, item metadataAutofillItem) {
	lookupCtx, cancel := context.WithTimeout(ctx, metadataAutofillLookupTimeout)
	track, err := a.resolver.LookupFresh(lookupCtx, metadata.Input{
		TrackName:  item.name,
		ArtistName: item.artist,
		AlbumName:  item.album,
		Duration:   item.duration,
	})
	cancel()

	switch {
	case track != nil:
		if persistErr := a.persist(item, track); persistErr != nil {
			if ctx.Err() == nil {
				a.logger.Warn("automatic metadata backfill could not store a resolved track",
					"track", item.name, "artist", item.artist, "error", persistErr)
			}
			return
		}
		a.logger.Debug("automatic metadata backfill resolved track",
			"track", item.name, "artist", item.artist, "track_id", item.trackID)
	case errors.Is(err, metadata.ErrInconclusive):
		// A throttle, a refusal, or a network failure says something about the
		// moment rather than about the song, so a requested track is retried.
		if item.attempts < metadataAutofillAttempts && ctx.Err() == nil {
			item.attempts++
			a.requeue(item)
		}
	case errors.Is(err, metadata.ErrNotFound):
		if item.trackID > 0 {
			if markErr := a.markChecked(item.trackID); markErr != nil && ctx.Err() == nil {
				a.logger.Warn("automatic metadata backfill could not record a miss", "error", markErr)
			}
		}
	default:
		if ctx.Err() == nil {
			a.logger.Warn("automatic metadata backfill lookup failed",
				"track", item.name, "artist", item.artist, "error", err)
		}
	}
}

// persist writes a provider answer back to the library.
//
// A row that was read from the database is enriched in place, so a provider
// returning a different release variant cannot create a near-duplicate of a
// track the library already holds. A requested track the library has no row for
// is inserted, which is what makes a repeat request a local hit.
func (a *metadataAutofill) persist(item metadataAutofillItem, track *db.Track) error {
	if track == nil {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), metadataAutofillWriteTimeout)
	defer cancel()

	merged := *track
	merged.MetadataChecked = true
	if item.trackID > 0 {
		tx, err := a.database.BeginTx(writeCtx, nil)
		if err != nil {
			return err
		}
		if err = db.MergeTrackMetadata(writeCtx, tx, item.trackID, merged); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	} else if _, err := db.UpsertTrackMetadata(writeCtx, a.database, merged); err != nil {
		return err
	}
	// Let the live server drop memoized answers, including a miss it may still
	// be holding for this track. A failure here only costs a stale entry until
	// the next restart, so it must not fail the write.
	if _, err := db.BumpMetadataRevision(writeCtx, a.database); err != nil && writeCtx.Err() == nil {
		a.logger.Warn("automatic metadata backfill could not signal a metadata revision", "error", err)
	}
	return nil
}

// markChecked records that every provider answered and none of them knew the
// track, so the sweep stops asking.
func (a *metadataAutofill) markChecked(trackID int64) error {
	writeCtx, cancel := context.WithTimeout(context.Background(), metadataAutofillWriteTimeout)
	defer cancel()
	tx, err := a.database.BeginTx(writeCtx, nil)
	if err != nil {
		return err
	}
	if err = db.MarkTrackMetadataChecked(writeCtx, tx, trackID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
