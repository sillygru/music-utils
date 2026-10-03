package pacer

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

// Waiter is the pacing contract every provider depends on. Both the in-process
// Pacer and the cross-process Shared pacer satisfy it, so a provider can be
// handed either without changing its own code.
type Waiter interface {
	Wait(ctx context.Context) error
}

// Class identifies who is asking for an upstream slot, which decides how
// aggressively the shared pacer serves them.
type Class int

const (
	// ClassUser is live API traffic. It is served immediately and its activity
	// closes the idle window that background work has to wait for.
	ClassUser Class = iota
	// ClassJob is background batch work. It is only served while the server is
	// idle, so a running job never competes with a real request.
	ClassJob
)

// DefaultIdleGap is how long the shared pacer waits after the last live request
// before serving background work. It is deliberately not zero: a job firing in
// the gap between two user requests would still add latency to the next one.
const DefaultIdleGap = 10 * time.Second

// Shared is a pacer whose state lives in SQLite, so the live server and any
// background job running on the same database take turns on one upstream host
// instead of each applying private pacing and doubling the real request rate.
//
// It also encodes priority. A live request claims a slot as soon as one is free
// and stamps last_user_at, which starts an idle window. Background work is only
// admitted once that window has passed without traffic, and it re-reads the
// lease after every wait, so a job that was mid-flight when a user arrived hands
// the slot back and re-queues instead of firing into that request. That wait is
// bounded, or a server in steady use would deny it forever.
//
// The two classes are paced at different rates, because what a request costs an
// upstream and what a batch pass may spend on it are not the same question. A
// user-facing lookup is waiting on this one answer, so it is served at the rate
// the provider's own client uses. A background job is working through a library
// and can afford to be slower than the thing it is competing with, and its rate
// is set to whatever the job needs rather than to the request path's latency.
// They share one lease and still take turns; they simply no longer have to agree
// on a single number to do it.
type Shared struct {
	db   *sql.DB
	name string
	// userInterval spaces live requests against each other, and jobInterval
	// spaces background work against itself. Each is used only by its own class.
	userInterval time.Duration
	jobInterval  time.Duration
	idleGap      time.Duration

	// userFallback and jobFallback pace locally if the coordination table is
	// unavailable, so a database problem degrades to private pacing instead of no
	// pacing. There is one per class because the degraded mode has to preserve
	// the same two rates the shared lease would have applied.
	userFallback *Pacer
	jobFallback  *Pacer
	mu           sync.Mutex
	usable       bool
	// disabledUntil is when a failed claim may be retried. A single transient
	// error, most often the live server holding the write lock, must not cost
	// this process shared pacing for the rest of its life: the alternative is
	// silently reverting to the doubled request rate that shared pacing exists
	// to prevent. Zero means coordination is believed to be working.
	disabledUntil time.Time
	// lastError records why shared pacing was abandoned, if it was.
	lastError error
}

// NewShared builds a Shared pacer for the named upstream host, spacing live
// requests at least userInterval apart and background work at least jobInterval
// apart. An idleGap of zero uses DefaultIdleGap.
//
// A jobInterval of zero or below falls back to userInterval, so an upstream the
// two classes should share a rate on does not have to say so twice. A nil
// database builds a pacer that only ever paces locally, which is what a caller
// without a metadata database wants.
func NewShared(database *sql.DB, name string, userInterval, jobInterval, idleGap time.Duration) *Shared {
	if idleGap <= 0 {
		idleGap = DefaultIdleGap
	}
	if jobInterval <= 0 {
		jobInterval = userInterval
	}
	shared := &Shared{
		db:           database,
		name:         name,
		userInterval: userInterval,
		jobInterval:  jobInterval,
		idleGap:      idleGap,
		userFallback: New(userInterval),
		jobFallback:  New(jobInterval),
	}
	shared.usable = database != nil
	return shared
}

// ForUser returns a Waiter for live API traffic.
func (s *Shared) ForUser() Waiter { return &sharedWaiter{shared: s, class: ClassUser} }

// ForJob returns a Waiter for background batch work, which yields to live
// traffic but is admitted anyway once the idle wait has been held back too many
// times.
func (s *Shared) ForJob() Waiter { return &sharedWaiter{shared: s, class: ClassJob} }

// Wait satisfies Waiter using the user class, so a Shared can stand in for a
// plain Pacer wherever live traffic is expected.
func (s *Shared) Wait(ctx context.Context) error { return s.ForUser().Wait(ctx) }

// intervalFor reports the spacing this class claims at.
func (s *Shared) intervalFor(class Class) time.Duration {
	if class == ClassUser {
		return s.userInterval
	}
	return s.jobInterval
}

// fallbackFor reports the local pacer used when the shared lease is unavailable,
// which has to match the rate the lease would have applied for this class.
func (s *Shared) fallbackFor(class Class) *Pacer {
	if class == ClassUser {
		return s.userFallback
	}
	return s.jobFallback
}

// idle returns how long live traffic must stay quiet before background work is
// admitted.
func (s *Shared) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idleGap
}

// available reports whether a claim should be attempted, re-enabling the shared
// lease once its cooldown has passed.
func (s *Shared) available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return false
	}
	if !s.usable {
		if time.Now().Before(s.disabledUntil) {
			return false
		}
		// The cooldown expired. Try the lease again rather than treating one
		// transient failure as permanent; a table that is genuinely gone keeps
		// failing and the cooldown simply repeats.
		s.usable = true
	}
	return true
}

// disable paces locally for the cooldown period after a claim failed.
func (s *Shared) disable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usable = false
	s.disabledUntil = time.Now().Add(sharedRetryCooldown)
}

// sharedRetryCooldown is how long a failed claim abandons the shared lease
// before retrying it.
//
// Long enough that a database briefly held by the live server does not cause a
// request to retry per call, short enough that a blip costs a fraction of a
// second of the cross-process guarantee rather than the rest of the process's
// life.
const sharedRetryCooldown = 30 * time.Second

func (s *Shared) setLastError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err
}

// Degraded reports whether a claim has failed recently enough that this pacer is
// currently pacing locally instead of on the shared lease, and why.
//
// Without an observable, a broken coordination table looks exactly like correct
// behaviour: the pacer quietly reverts to private pacing and the server and any
// job stop taking turns.
func (s *Shared) Degraded() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.usable && s.lastError != nil, s.lastError
}

// sharedWaiter binds a Shared pacer to one caller class.
type sharedWaiter struct {
	shared *Shared
	class  Class
}

func (w *sharedWaiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !w.shared.available() {
		return w.shared.fallbackFor(w.class).Wait(ctx)
	}
	// The job defers for at most this long, then proceeds anyway. A user claim
	// serializes against user_next_at and ignores next_at, so a job that stops
	// deferring costs a user nothing; deferring without limit means a server in
	// steady use never grants the idle window and no batch work ever runs.
	started := time.Now()
	for {
		lease, err := w.shared.claim(ctx, w.class)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The coordination table is missing or locked. Pace locally for the
			// cooldown rather than hammering the database on every request, and
			// retry the lease after it, so a transient lock costs a moment of
			// cross-process coordination instead of all of it. The reason is
			// retained so the degradation is observable.
			w.shared.setLastError(err)
			w.shared.disable()
			return w.shared.fallbackFor(w.class).Wait(ctx)
		}
		if err := sleepUntil(ctx, time.UnixMilli(lease.AdmitAt)); err != nil {
			return err
		}
		if w.class != ClassJob {
			return nil
		}
		if time.Since(started) >= maxIdleWait {
			return nil
		}
		// Background work re-reads the lease after waiting, because the value
		// claim returned describes live traffic as of the moment it was read,
		// before the wait. Only a fresh read can observe a user who arrived
		// while this job was queued, and that user is the whole reason the
		// re-check exists: without it the job fires into a request path it was
		// supposed to be standing aside for.
		//
		// Re-reading can cost one reserved slot, since claim has already pushed
		// next_at out. That is a throughput price, not a correctness one, and it
		// errs in the safe direction: the lease only ever moves later.
		lastUserAt, readErr := w.shared.readLastUserAt(ctx)
		if readErr != nil {
			// The lease is unreadable, so there is no evidence the server is
			// idle. Standing aside is the conservative reading of that.
			lastUserAt = time.Now().UnixMilli()
		}
		if w.shared.idleFor(lastUserAt) {
			return nil
		}
	}
}

// maxIdleWait bounds how long one call defers for the server's idle window
// before taking its slot anyway.
const maxIdleWait = 20 * time.Second

// maxJobReserve caps how far into the future a job may reserve a slot on the
// shared lease.
//
// Without a cap the lease compounds: a deferred job reserves lastUserAt+idleGap,
// pushes next_at past it, and on its next attempt reserves further out again, so
// the waits grow 10s, 12s, 24s, 48s. That is what made a busy server look like a
// hang rather than a slow queue. Clamping the reservation keeps a deferred job
// from pushing the lease it is queued behind.
const maxJobReserve = 5 * time.Second

// claim reserves the next slot for a caller.
//
// The read-modify-write runs inside one transaction that takes its write lock
// up front (BEGIN IMMEDIATE, configured on the handle). That is what makes the
// lease correct across processes: with a deferred transaction two processes can
// both read the same lease and race to update it, and SQLite resolves that by
// failing one of them rather than by serializing them.
func (s *Shared) claim(ctx context.Context, class Class) (db.PacerLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return db.PacerLease{}, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()
	nowMillis := now.UnixMilli()
	interval := s.intervalFor(class).Milliseconds()

	var nextAt, userNextAt, lastUserAt int64
	row := tx.QueryRowContext(ctx,
		"SELECT next_at, user_next_at, last_user_at FROM upstream_pacer WHERE name = ?", s.name)
	switch err := row.Scan(&nextAt, &userNextAt, &lastUserAt); {
	case errors.Is(err, sql.ErrNoRows):
		nextAt, userNextAt, lastUserAt = 0, 0, 0
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO upstream_pacer (name, next_at, user_next_at, last_user_at) VALUES (?, 0, 0, 0)", s.name); err != nil {
			return db.PacerLease{}, err
		}
	case err != nil:
		return db.PacerLease{}, err
	}

	admit := nowMillis
	if class == ClassUser {
		// Live traffic serializes only against other live traffic, on its own
		// lease. It deliberately ignores next_at, which a background job may
		// have booked far ahead, so a user is never delayed by the job.
		if userNextAt > admit {
			admit = userNextAt
		}
		lastUserAt = nowMillis
		userNextAt = admit + interval
		// Record the usage on the shared lease too, so a job cannot claim the
		// instant a user just used.
		if shared := admit + interval; shared > nextAt {
			nextAt = shared
		}
	} else {
		// Background work only runs while no live request is pending, and also
		// honors the shared lease so it cannot stack on live traffic.
		if lastUserAt > 0 {
			floor := lastUserAt + s.idle().Milliseconds()
			// A job that is only waiting out traffic must not be able to push
			// the shared lease arbitrarily far ahead: each deferred attempt would
			// reserve further out than the last, and the waits would compound
			// until the job is effectively never admitted.
			if limit := nowMillis + maxJobReserve.Milliseconds(); floor > limit {
				floor = limit
			}
			if floor > admit {
				admit = floor
			}
		}
		if nextAt > admit {
			admit = nextAt
		}
		nextAt = admit + interval
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE upstream_pacer SET next_at = ?, user_next_at = ?, last_user_at = ? WHERE name = ?",
		nextAt, userNextAt, lastUserAt, s.name); err != nil {
		return db.PacerLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.PacerLease{}, err
	}
	return db.PacerLease{AdmitAt: admit, LastUserAt: lastUserAt}, nil
}

// readLastUserAt re-reads the last time live traffic used this upstream.
//
// claim returns a snapshot taken before any wait, so it cannot answer "is the
// server busy now". This is the fresh read the job class needs once it has
// waited out its reserved slot.
func (s *Shared) readLastUserAt(ctx context.Context) (int64, error) {
	var lastUserAt int64
	err := s.db.QueryRowContext(ctx,
		"SELECT last_user_at FROM upstream_pacer WHERE name = ?", s.name).Scan(&lastUserAt)
	if errors.Is(err, sql.ErrNoRows) {
		// No row means no traffic has ever been recorded, which is idle.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return lastUserAt, nil
}

// idleFor reports whether the server has been quiet long enough for background
// work, given the last user activity observed when the slot was claimed.
func (s *Shared) idleFor(lastUserAt int64) bool {
	if lastUserAt == 0 {
		return true
	}
	elapsed := time.Now().UnixMilli() - lastUserAt
	return elapsed >= s.idle().Milliseconds()
}

func sleepUntil(ctx context.Context, target time.Time) error {
	wait := time.Until(target)
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Compile-time check that a Shared pacer can be used anywhere a plain Pacer is.
var (
	_ Waiter = (*Pacer)(nil)
	_ Waiter = (*Shared)(nil)
)
