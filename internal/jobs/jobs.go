package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/sillygru/music-utils/internal/config"
)

// ErrUnknownJob is returned when --run-job names a job that is not registered.
var ErrUnknownJob = errors.New("unknown job")

// Job is one batch maintenance task. Jobs are registered at init time and
// discovered by `music-utils --jobs`.
type Job interface {
	// Name is the identifier passed to --run-job.
	Name() string
	// Summary is the one-line description shown by --jobs.
	Summary() string
	// Flags documents the job-specific flags it accepts, for --jobs output.
	Flags() string
	// Run executes the job. Implementations must honor ctx cancellation and
	// must not assume they own the databases: the live server may be reading
	// and writing the same files.
	Run(ctx context.Context, opts Options) error
}

// FlagDescriber is an optional interface a job implements to restate the help text
// of a flag the command layer defines for every job.
//
// The flags are shared, so their default wording has to be written for jobs in
// general and ends up imprecise for any one of them. The lyrics job works on songs
// and paces itself per provider rather than per worker, so both of its units differ
// from the metadata job's, and describing them in the metadata job's terms would
// tell an operator the wrong thing about what they are limiting.
type FlagDescriber interface {
	// FlagDescriptions maps flag names to the help text to use instead.
	FlagDescriptions() map[string]string
}

// Options carries everything a job needs. Databases are opened by the caller so
// every job shares the same connection, busy-timeout, and WAL behavior as the
// server.
type Options struct {
	Config     config.Config
	MetadataDB *sql.DB
	LyricsDB   *sql.DB
	CoverDB    *sql.DB
	// MetadataDBPath and LyricsDBPath are the files the handles above were opened
	// from, so a job can name the right one when it reports a failure. They reflect
	// the command line overrides, not just the environment.
	MetadataDBPath string
	LyricsDBPath   string
	Out            io.Writer
	ErrOut         io.Writer

	// Concurrency is how many items the job may work on at once. For a job that
	// walks each item through several upstreams in turn, that is items in flight
	// rather than requests in flight.
	Concurrency int
	// RatePerMinute caps how many work items the job starts per minute across all
	// workers. Zero means "no extra ceiling beyond provider pacing".
	//
	// It counts work items, not upstream requests. A job that fans out to several
	// providers spends one request per provider per item, and those requests are
	// already spaced by each provider's own pacer; adding a global request ceiling
	// on top would just make this flag the binding constraint again.
	RatePerMinute int
	// Limit caps how many work items are processed; zero means no cap.
	Limit int
	// DryRun reports what would change without writing to the databases.
	DryRun bool
	// Refresh re-fetches work a provider has already settled instead of only
	// work that was never attempted.
	//
	// It is a separate field from RefreshOlderThan because the two states are
	// genuinely different and Go's flag package cannot tell them apart: "-refresh"
	// alone and "-refresh 0" both parse as a zero duration, but the first means
	// "re-fetch everything, newest work last" and the second is the same thing
	// only by coincidence. Collapsing them would make it impossible for a caller
	// to ask for a bare refresh without also allowing a zero cutoff to mean
	// "never refresh".
	Refresh bool
	// RefreshOlderThan is how old a settled answer must be to count as stale
	// when Refresh is set. Zero means every settled item is stale, so a bare
	// -refresh re-fetches the whole library.
	RefreshOlderThan time.Duration
	// MaxWriteErrors is how many consecutive failed write batches a job
	// tolerates before stopping. Zero or less disables the abort. This catches
	// a database that becomes unwritable after startup, which a one-time
	// startup check cannot see.
	MaxWriteErrors int
	// Progress renders live lane output. Jobs must call Start and defer stop.
	Progress *Progress
	// UserIdleGap is how long the live server must stay quiet before a job is
	// allowed to touch a shared upstream. Zero uses the pacer default.
	UserIdleGap time.Duration
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Job{}
)

// Register adds a job to the registry. It panics on a duplicate or empty name
// because both are programming errors caught immediately at init time.
func Register(job Job) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if job == nil {
		panic("jobs: Register called with nil job")
	}
	if job.Name() == "" {
		panic("jobs: Register called with an empty job name")
	}
	if _, exists := registry[job.Name()]; exists {
		panic(fmt.Sprintf("jobs: job %q registered twice", job.Name()))
	}
	registry[job.Name()] = job
}

// Lookup returns the registered job with the given name.
func Lookup(name string) (Job, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	job, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownJob, name)
	}
	return job, nil
}

// All returns every registered job, sorted by name so --jobs output is stable.
func All() []Job {
	registryMu.RLock()
	defer registryMu.RUnlock()
	list := make([]Job, 0, len(registry))
	for _, job := range registry {
		list = append(list, job)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
	return list
}

// PrintList writes the --jobs catalog: every job with its summary and flags.
func PrintList(out io.Writer) {
	jobs := All()
	if len(jobs) == 0 {
		fmt.Fprintln(out, "No jobs are registered.")
		return
	}
	fmt.Fprintf(out, "Available jobs (%d):\n\n", len(jobs))
	for _, job := range jobs {
		fmt.Fprintf(out, "  %s\n", job.Name())
		fmt.Fprintf(out, "      %s\n", job.Summary())
		if flags := job.Flags(); flags != "" {
			fmt.Fprintf(out, "      flags: %s\n", flags)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "Run one with: music-utils --run-job <name>\n")
}
