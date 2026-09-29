package httpserver

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
)

// jobWatchInterval is how often the server checks whether a background job has
// committed new metadata. It is deliberately coarse: the check is a single-row
// read, and reacting within a few seconds is plenty for a cache whose entries
// otherwise live for 24 hours.
const jobWatchInterval = 5 * time.Second

// jobWatcher keeps the server's memoized metadata in step with the database.
//
// The resolver memoizes both hits and misses, and a miss is cached for 24 hours.
// A background job resolving a track writes the answer straight to the database,
// which this process never sees, so without invalidation the server would keep
// serving its own stale 404 for a song that is now cached. Watching the
// revision counter the job bumps on each commit closes that window without
// having to restart the service.
type jobWatcher struct {
	metadataDB  *sql.DB
	resolver    *metadata.Resolver
	logger      *slog.Logger
	interval    time.Duration
	stopCh      chan struct{}
	stopOnce    sync.Once
	stoppedCh   chan struct{}
	lastSeen    int64
	initialised bool
	// sawBump distinguishes "no bump yet" from "bumped at time zero", so a
	// database whose stamp is still the zero value is not mistaken for one
	// that has just been signalled.
	sawBump bool
}

// newJobWatcher builds a watcher for one resolver. It returns nil when there is
// no metadata database, which is the case in tests using in-memory stores.
func newJobWatcher(metadataDB *sql.DB, resolver *metadata.Resolver, logger *slog.Logger) *jobWatcher {
	if metadataDB == nil || resolver == nil {
		return nil
	}
	return &jobWatcher{
		metadataDB: metadataDB,
		resolver:   resolver,
		logger:     logger,
		interval:   jobWatchInterval,
		stopCh:     make(chan struct{}),
		stoppedCh:  make(chan struct{}),
	}
}

// Start begins polling in the background.
func (w *jobWatcher) Start() {
	if w == nil {
		return
	}
	go w.run()
}

func (w *jobWatcher) run() {
	defer close(w.stoppedCh)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Record the starting state without invalidating: everything memoized so
	// far is consistent with what is already on disk.
	if state, err := db.ReadCoordination(context.Background(), w.metadataDB); err == nil {
		w.lastSeen = state.RevisionBumpedAt
		w.sawBump = state.RevisionBumpedAt > 0
		w.initialised = true
	} else if w.logger != nil {
		w.logger.Warn("job coordination unavailable; metadata cache will not follow background jobs", "error", err)
	}

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.check()
		}
	}
}

// check reads the current revision and invalidates when a job has advanced it.
func (w *jobWatcher) check() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state, err := db.ReadCoordination(ctx, w.metadataDB)
	if err != nil {
		return
	}
	if !w.initialised {
		w.lastSeen = state.RevisionBumpedAt
		w.sawBump = state.RevisionBumpedAt > 0
		w.initialised = true
		return
	}
	// Watch the bump stamp rather than the counter itself. A job throttles its
	// bumps, so the counter can climb many times between notifications while the
	// stamp holds still; comparing the counter would invalidate the cache on
	// every poll and keep it perpetually cold during a long backfill.
	if state.RevisionBumpedAt == 0 {
		return
	}
	if w.sawBump && state.RevisionBumpedAt == w.lastSeen {
		return
	}
	w.lastSeen = state.RevisionBumpedAt
	w.sawBump = true
	w.resolver.Invalidate()
	if w.logger != nil {
		w.logger.Info("metadata cache invalidated after background job write",
			"revision", state.MetadataRevision, "job", state.ActiveJob)
	}
}

// Stop halts polling and waits for the goroutine to finish.
func (w *jobWatcher) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.stoppedCh
}
