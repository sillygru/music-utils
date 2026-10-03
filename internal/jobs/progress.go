// Package jobs provides the batch maintenance commands exposed by the
// music-utils CLI: `music-utils --jobs` lists them and
// `music-utils --run-job <name>` executes one.
package jobs

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// Done returns how many units of work the lane has finished processing.
//
// The counters are guarded by the owning Progress's mutex, which is also what
// Advance and the renderer take, so reading them here cannot race a worker.
func (l *Lane) Done() int64 {
	if l.progress == nil {
		return l.done
	}
	l.progress.mu.Lock()
	defer l.progress.mu.Unlock()
	return l.done
}

// Lane is one independently reported stream of work inside a job. A job may
// run several lanes at once (one per worker, per provider, or per data type)
// and each keeps its own counters so a slow lane never hides a busy one.
type Lane struct {
	// Name labels the lane in the rendered output.
	Name string
	// Total is the expected unit count, or 0 when the job streams work and the
	// final size is not yet known.
	Total int64

	// progress is the renderer that owns this lane's counters, set by
	// NewProgress. A lane with no renderer is inert and may be read directly.
	progress *Progress

	done      int64
	succeeded int64
	missed    int64
	throttled int64
	failed    int64
	current   string
}

// Progress renders the live state of a job's lanes. It is safe for concurrent
// use: lanes report progress from worker goroutines while a single renderer
// goroutine draws.
type Progress struct {
	mu     sync.Mutex
	out    io.Writer
	lanes  []*Lane
	closed bool

	// lines is the height of the last frame, used to redraw in place.
	lines int
	// interactive enables in-place redrawing. When false the renderer only
	// emits periodic snapshots so piped output and CI logs stay readable.
	interactive bool
	// interval is the redraw cadence.
	interval time.Duration
	// started anchors the elapsed-time and throughput readouts.
	started time.Time
	done    chan struct{}
	// snapshots counts rendered frames, for plain output.
	snapshots int
	// nextSnapshot is the earliest time the next plain-output frame is due.
	nextSnapshot time.Time
	// notices are diagnostic lines to print with the frame.
	notices []string
	// noticesShown is how many notices have already been printed, so a growing
	// list does not reprint the same lines on every frame.
	noticesShown int
	// writeErrors is the writer's failure count, rendered in the frame.
	writeErrors int64
	// dirty forces a repaint when a notice or error count changed but the
	// interval has not elapsed yet.
	dirty bool
}

// snapshotInterval is how often plain (non-terminal) output prints a frame.
const snapshotInterval = 5 * time.Second

// NewProgress creates a renderer writing to out. Lanes are rendered in order,
// one per line, followed by a totals line.
func NewProgress(out io.Writer, lanes []*Lane) *Progress {
	progress := &Progress{
		out:         out,
		lanes:       lanes,
		interactive: IsTerminal(out),
		interval:    200 * time.Millisecond,
		started:     time.Now(),
		done:        make(chan struct{}),
		snapshots:   0,
	}
	// Link the lanes back to the renderer so their counters are guarded by the
	// same mutex that mutates them.
	for _, lane := range lanes {
		if lane != nil {
			lane.progress = progress
		}
	}
	return progress
}

// IsTerminal reports whether w is an interactive terminal. Rendering is only
// in-place when it is; otherwise the renderer falls back to periodic lines.
func IsTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Notice records a diagnostic line to show with the live output.
//
// Diagnostics are attached to the renderer rather than written straight to the
// error stream because a job's workers and its renderer both write while the
// renderer is redrawing in place. An unsynchronized write lands between the
// erase sequence and the rewritten frame, which corrupts the display and can
// hide the message entirely. Buffering them in the renderer keeps every line
// visible and keeps the frame's line count correct.
func (p *Progress) Notice(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.notices = append(p.notices, fmt.Sprintf(format, args...))
	if len(p.notices) > maxNotices {
		p.notices = p.notices[len(p.notices)-maxNotices:]
	}
	p.dirty = true
}

// maxNotices caps the retained diagnostic lines. A run against a failing
// provider can produce thousands of identical errors, and printing all of them
// buries the frame that says what is actually happening.
const maxNotices = 5

// maxReportedNotices is how many diagnostics are printed verbatim. The rest are
// counted in the frame, so the operator sees both that errors are happening and
// the first few that explain them.
const maxReportedNotices = 3

// SetWriteErrors publishes the writer's failure count to the frame.
func (p *Progress) SetWriteErrors(count int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writeErrors == count {
		return
	}
	p.writeErrors = count
	p.dirty = true
}

// Outcome classifies how one unit of work resolved.
type Outcome int

const (
	// OutcomeSucceeded means the unit produced a usable result.
	OutcomeSucceeded Outcome = iota
	// OutcomeMissing means upstream had no result for the unit; it is a real
	// negative, not a failure.
	OutcomeMissing
	// OutcomeThrottled means upstream rate limited the unit. The work is not
	// lost, and the lane leaves it for a later pass.
	OutcomeThrottled
	// OutcomeFailed means the unit errored for a non-rate-limit reason.
	OutcomeFailed
)

// Advance moves a lane forward by one unit and classifies the result.
func (p *Progress) Advance(lane *Lane, outcome Outcome, current string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	lane.done++
	switch outcome {
	case OutcomeSucceeded:
		lane.succeeded++
	case OutcomeMissing:
		lane.missed++
	case OutcomeThrottled:
		lane.throttled++
	case OutcomeFailed:
		lane.failed++
	}
	if current != "" {
		lane.current = current
	}
}

// TotalDone returns the sum of every lane's completed units.
func (p *Progress) TotalDone() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sum int64
	for _, lane := range p.lanes {
		sum += lane.done
	}
	return sum
}

// Total returns the sum of every lane's expected units.
func (p *Progress) Total() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sum int64
	for _, lane := range p.lanes {
		sum += lane.Total
	}
	return sum
}

// Start launches the background renderer. The returned function stops it and
// prints a final frame.
func (p *Progress) Start() func() {
	ticker := time.NewTicker(p.interval)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.render()
			case <-p.done:
				return
			}
		}
	}()
	return func() {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		p.closed = true
		p.mu.Unlock()
		close(p.done)
		<-stopped
		p.renderFinal()
	}
}

func (p *Progress) render() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	frame := p.frameLocked()
	if p.interactive {
		// Erase the previous frame, then rewrite it in place.
		if p.lines > 0 {
			fmt.Fprint(p.out, strings.Repeat("\033[1A\033[2K", p.lines))
		}
		fmt.Fprint(p.out, frame)
		p.lines = strings.Count(frame, "\n")
		p.noticesShown = len(p.notices)
		return
	}
	// Non-interactive: emit a snapshot at a human cadence instead of a
	// clearing sequence that would fill a log file with escape codes.
	p.snapshots++
	now := time.Now()
	if p.snapshots != 1 && now.Before(p.nextSnapshot) {
		return
	}
	p.nextSnapshot = now.Add(snapshotInterval)
	p.printPendingNoticesLocked()
	fmt.Fprint(p.out, frame)
}

// printPendingNoticesLocked writes diagnostics that have not been shown yet. The
// caller must hold p.mu so the lines cannot land between an erase and a repaint.
func (p *Progress) printPendingNoticesLocked() {
	if p.noticesShown >= len(p.notices) {
		return
	}
	// Report the newest lines and count what was suppressed, so an operator
	// always knows the total rather than believing the output was complete.
	start := p.noticesShown
	pending := p.notices[start:]
	if len(pending) > maxReportedNotices {
		for _, notice := range pending[len(pending)-maxReportedNotices:] {
			fmt.Fprintf(p.out, "  ! %s\n", notice)
		}
		fmt.Fprintf(p.out, "  ! and %d earlier message(s) suppressed\n", start+len(pending)-maxReportedNotices)
	} else {
		for _, notice := range pending {
			fmt.Fprintf(p.out, "  ! %s\n", notice)
		}
	}
	p.noticesShown = len(p.notices)
}

func (p *Progress) renderFinal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.interactive && p.lines > 0 {
		fmt.Fprint(p.out, strings.Repeat("\033[1A\033[2K", p.lines))
	}
	// Every retained notice is printed at the end: the final frame is the last
	// thing on screen, so anything dropped here would be lost with the redraw.
	for i, notice := range p.notices {
		if i >= maxReportedNotices {
			fmt.Fprintf(p.out, "  ! and %d earlier message(s) suppressed\n", len(p.notices)-maxReportedNotices)
			break
		}
		fmt.Fprintf(p.out, "  ! %s\n", notice)
	}
	p.noticesShown = len(p.notices)
	fmt.Fprint(p.out, p.frameLocked())
}

// barWidth is the width of the drawn bar in cells.
const barWidth = 28

// frameLocked builds the whole rendered frame. Callers must hold p.mu.
func (p *Progress) frameLocked() string {
	// The ETA needs the unrounded duration: rounding to whole seconds first
	// would make a run that started half a second ago look like it had taken
	// zero, and the division below would divide by zero.
	raw := time.Since(p.started)
	elapsed := raw.Round(time.Second)
	var done, total, succeeded, missed, throttled, failed int64
	for _, lane := range p.lanes {
		done += lane.done
		total += lane.Total
		succeeded += lane.succeeded
		missed += lane.missed
		throttled += lane.throttled
		failed += lane.failed
	}

	var builder strings.Builder
	for _, lane := range p.lanes {
		builder.WriteString(renderLane(lane, elapsed))
		builder.WriteByte('\n')
	}
	builder.WriteString(fmt.Sprintf("  %-13s %s  done %d/%s  ok %d  miss %d  throttled %d  failed %d%s  elapsed %s  %s\n",
		"total", bar(done, total), done, humanCount(total), succeeded, missed, throttled, failed,
		renderWriteErrorsLocked(p.writeErrors), elapsed, formatETA(done, total, raw)))
	return builder.String()
}

// formatETA estimates the time remaining from the average rate so far.
//
// A long backfill is measured in hours, so without this the operator has no way
// to tell whether to wait or to come back later. The estimate is omitted rather
// than guessed when there is not yet enough information: a rate needs at least
// one completed unit and a non-zero elapsed time, and a streaming job with an
// unknown total has no meaningful finish to project onto.
func formatETA(done, total int64, elapsed time.Duration) string {
	if done <= 0 || total <= 0 || elapsed <= 0 {
		return "eta --"
	}
	remaining := total - done
	if remaining <= 0 {
		return "eta done"
	}
	perUnit := float64(elapsed) / float64(done)
	eta := time.Duration(float64(remaining) * perUnit)
	// A run that is visibly stalled (upstream throttling, a retry storm) would
	// otherwise project a finish days out, which reads as a bug rather than a
	// warning. Cap the display and let the elapsed counter keep climbing.
	const maxETA = 100 * time.Hour
	if eta > maxETA {
		eta = maxETA
	}
	return "eta " + formatDuration(eta)
}

// formatDuration renders an estimate in the largest two units that matter, so
// "14h 20m" stays readable where "51600m0s" does not.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	if d < time.Hour {
		minutes := int(d / time.Minute)
		// Truncate seconds to a multiple of ten: an estimate precise to the
		// second implies a confidence the underlying rate does not have.
		seconds := int(d%time.Minute/time.Second/10) * 10
		if seconds == 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	}
	hours := int(d / time.Hour)
	minutes := int(d%time.Hour) / int(time.Minute)
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, minutes)
}

func renderLane(lane *Lane, elapsed time.Duration) string {
	counter := fmt.Sprintf("%d/%s", lane.done, humanCount(lane.Total))
	current := lane.current
	if current != "" {
		current = " " + truncate(current, 44)
	}
	return fmt.Sprintf("  %-13s %s  %-17s ok %d  miss %d  throttled %d  failed %d%s\n",
		truncate(lane.Name, 13), bar(lane.done, lane.Total), counter,
		lane.succeeded, lane.missed, lane.throttled, lane.failed, current)
}

// renderWriteErrorsLocked formats the write-failure field of the totals line. It
// is only present once something has failed, so a healthy run keeps its frame
// unchanged and the field is meaningful rather than always-zero noise.
func renderWriteErrorsLocked(failures int64) string {
	if failures <= 0 {
		return ""
	}
	return fmt.Sprintf("  writes-failed %d", failures)
}

// bar renders a fixed-width progress bar. An unknown total (zero) draws a
// moving indicator rather than a misleading empty bar.
func bar(done, total int64) string {
	if total <= 0 {
		return fmt.Sprintf("[%s] %5.1f%%", strings.Repeat("-", barWidth/2), 0.0)
	}
	fraction := float64(done) / float64(total)
	if fraction > 1 {
		fraction = 1
	}
	if fraction < 0 {
		fraction = 0
	}
	filled := int(fraction * barWidth)
	return fmt.Sprintf("[%s%s] %5.1f%%", strings.Repeat("=", filled), strings.Repeat(" ", barWidth-filled), fraction*100)
}

// humanCount renders large counters with a thousands separator.
func humanCount(value int64) string {
	if value <= 0 {
		return "?"
	}
	return exactCount(value)
}

// exactCount formats a finished count.
//
// It exists because humanCount renders zero as "?", which is the right affordance
// for a lane that has not started yet but reads as a missing value in a summary
// line: a run that found nothing would otherwise report "? lyrics fetched" and look
// like it had lost the count.
func exactCount(value int64) string {
	if value < 0 {
		return "?"
	}
	digits := fmt.Sprintf("%d", value)
	var builder strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			builder.WriteByte(',')
		}
		builder.WriteRune(digit)
	}
	return builder.String()
}

// paceText renders a pace interval as the rate a run header claims to be working
// at, in whichever unit is honest for that interval.
//
// A sub-second interval reads best as a whole req/s figure, and rounding up keeps
// the claim on the safe side of the truth. An interval of a second or more has no
// whole req/s value at all: a 2s interval is half a request per second, and
// rounding that to "1 req/s" would state twice the rate the run is actually allowed
// to use, which is the one thing a rate line must never do. Past a second the
// interval is therefore stated directly, which stays exact for a value that is not
// a round number of seconds rather than only for the ones that are.
func paceText(interval time.Duration) string {
	switch {
	case interval <= 0:
		return "unpaced"
	case interval == time.Second:
		return "1 req/s"
	case interval < time.Second:
		return fmt.Sprintf("%d req/s", int(math.Ceil(float64(time.Second)/float64(interval))))
	case interval%time.Second == 0:
		return fmt.Sprintf("1 req/%ds", int(interval/time.Second))
	default:
		return fmt.Sprintf("1 req/%s", interval)
	}
}

func truncate(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 1 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}
