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

// Options carries everything a job needs. Databases are opened by the caller so
// every job shares the same connection, busy-timeout, and WAL behavior as the
// server.
type Options struct {
	Config     config.Config
	MetadataDB *sql.DB
	LyricsDB   *sql.DB
	CoverDB    *sql.DB
	Out        io.Writer
	ErrOut     io.Writer

	// Concurrency is the number of parallel workers the job may use.
	Concurrency int
	// RatePerMinute caps total upstream requests per minute across all
	// workers. Zero means "no extra ceiling beyond provider pacing".
	RatePerMinute int
	// Limit caps how many work items are processed; zero means no cap.
	Limit int
	// DryRun reports what would change without writing to the databases.
	DryRun bool
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
