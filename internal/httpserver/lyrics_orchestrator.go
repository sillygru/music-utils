package httpserver

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/db"
)

const lyricsResponseWait = 3 * time.Second

// lyricsLookupResult is the normalized result shared by all synchronous and
// background lyrics providers. A result may contain ordinary lyrics, a rich
// variant, or both.
type lyricsLookupResult struct {
	track  *db.Track
	lyrics *db.Lyrics
	rich   *db.RichLyrics

	err      error
	status   int
	retry    int
	upstream time.Duration
}

func (r lyricsLookupResult) usable() bool {
	return r.track != nil && ((r.lyrics != nil && lyricsAvailable(r.lyrics)) || r.rich != nil)
}

func (r lyricsLookupResult) quality() int {
	if r.rich != nil {
		return 3
	}
	if r.lyrics != nil && r.lyrics.SyncedLyrics != "" {
		return 2
	}
	if r.lyrics != nil && (r.lyrics.PlainLyrics != "" || r.lyrics.Instrumental) {
		return 1
	}
	return 0
}

func betterLyricsLookupResult(current, candidate lyricsLookupResult) lyricsLookupResult {
	if !candidate.usable() {
		return current
	}
	if !current.usable() || candidate.quality() > current.quality() {
		return candidate
	}
	// Prefer a result that enriches an existing result with a second variant.
	if current.track == nil || candidate.track == nil {
		return current
	}
	if candidate.track == nil {
		candidate.track = current.track
	}
	if current.lyrics == nil || !lyricsAvailable(current.lyrics) {
		candidate.lyrics = current.lyrics
	}
	if current.rich == nil {
		candidate.rich = current.rich
	}
	return candidate
}

type lyricsLookupJob struct {
	done chan struct{}
	wake chan struct{}

	mu     sync.Mutex
	best   lyricsLookupResult
	closed bool
	cancel context.CancelFunc
}

func (j *lyricsLookupJob) publish(result lyricsLookupResult) {
	if !result.usable() && result.err == nil {
		return
	}
	j.mu.Lock()
	if result.err != nil && j.best.err == nil {
		j.best.err = result.err
		j.best.status = result.status
		j.best.retry = result.retry
		j.best.upstream = result.upstream
	}
	if !result.usable() {
		j.mu.Unlock()
		select {
		case j.wake <- struct{}{}:
		default:
		}
		return
	}
	previous := j.best
	j.best = betterLyricsLookupResult(j.best, result)
	if j.best.upstream <= 0 {
		j.best.upstream = result.upstream
	}
	changed := j.best.quality() != previous.quality() || j.best.track == nil
	j.mu.Unlock()
	if changed {
		select {
		case j.wake <- struct{}{}:
		default:
		}
	}
}

func (j *lyricsLookupJob) snapshot() lyricsLookupResult {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.best
}

// lyricsLookupGroup guarantees one provider fan-out per canonical key. The
// background context is independent from the request context, so provider
// calls can finish and persist after the three-second response window.
type lyricsLookupGroup struct {
	mu   sync.Mutex
	jobs map[string]*lyricsLookupJob
}

func newLyricsLookupGroup() *lyricsLookupGroup {
	return &lyricsLookupGroup{jobs: make(map[string]*lyricsLookupJob)}
}

func (g *lyricsLookupGroup) lookup(ctx context.Context, key string, start func(context.Context, func(lyricsLookupResult))) lyricsLookupResult {
	g.mu.Lock()
	job, exists := g.jobs[key]
	if !exists {
		jobCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		job = &lyricsLookupJob{done: make(chan struct{}), wake: make(chan struct{}, 1), cancel: cancel}
		g.jobs[key] = job
		go func() {
			defer close(job.done)
			defer cancel()
			defer func() {
				g.mu.Lock()
				delete(g.jobs, key)
				g.mu.Unlock()
			}()
			start(jobCtx, job.publish)
		}()
	}
	g.mu.Unlock()

	timer := time.NewTimer(lyricsResponseWait)
	defer timer.Stop()
	for {
		if result := job.snapshot(); result.usable() {
			return result
		}
		select {
		case <-job.wake:
		case <-job.done:
			return job.snapshot()
		case <-timer.C:
			return job.snapshot()
		case <-ctx.Done():
			return job.snapshot()
		}
	}
}

func (g *lyricsLookupGroup) stop() {
	g.mu.Lock()
	jobs := make([]*lyricsLookupJob, 0, len(g.jobs))
	for _, job := range g.jobs {
		jobs = append(jobs, job)
	}
	g.mu.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
}

func firstJobError(errs []error) error {
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	return nil
}
