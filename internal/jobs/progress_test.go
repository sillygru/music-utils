package jobs

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBarRendersProgress(t *testing.T) {
	tests := []struct {
		name     string
		done     int64
		total    int64
		contains string
	}{
		{"empty", 0, 100, "0.0%"},
		{"half", 50, 100, "50.0%"},
		{"complete", 100, 100, "100.0%"},
		{"overshoot clamps", 150, 100, "100.0%"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bar(tc.done, tc.total)
			if !strings.Contains(got, tc.contains) {
				t.Fatalf("bar(%d,%d) = %q, expected it to contain %q", tc.done, tc.total, got, tc.contains)
			}
		})
	}
}

func TestBarWithUnknownTotalDoesNotDivideByZero(t *testing.T) {
	// A streaming job may not know its total yet; the bar must still render.
	got := bar(7, 0)
	if !strings.Contains(got, "%") {
		t.Fatalf("expected a rendered bar for an unknown total, got %q", got)
	}
}

func TestHumanCount(t *testing.T) {
	tests := map[int64]string{
		0:       "?",
		7:       "7",
		999:     "999",
		1000:    "1,000",
		20288:   "20,288",
		1234567: "1,234,567",
	}
	for value, want := range tests {
		if got := humanCount(value); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("expected no truncation, got %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "abcd…" {
		t.Errorf("truncate = %q, want %q", got, "abcd…")
	}
	// Multi-byte runes must not be split mid-character.
	if got := truncate("日本語のタイトル", 4); got != "日本語…" {
		t.Errorf("truncate = %q, want %q", got, "日本語…")
	}
}

func TestProgressAdvanceCountsOutcomes(t *testing.T) {
	lane := &Lane{Name: "lane-1", Total: 4}
	progress := NewProgress(&bytes.Buffer{}, []*Lane{lane})

	progress.Advance(lane, OutcomeSucceeded, "one")
	progress.Advance(lane, OutcomeMissing, "two")
	progress.Advance(lane, OutcomeThrottled, "three")
	progress.Advance(lane, OutcomeFailed, "four")

	if lane.Done() != 4 {
		t.Errorf("expected 4 done, got %d", lane.Done())
	}
	if lane.succeeded != 1 || lane.missed != 1 || lane.throttled != 1 || lane.failed != 1 {
		t.Errorf("unexpected counters: ok=%d miss=%d throttled=%d failed=%d",
			lane.succeeded, lane.missed, lane.throttled, lane.failed)
	}
	if lane.current != "four" {
		t.Errorf("expected the current item to be tracked, got %q", lane.current)
	}
	if progress.TotalDone() != 4 {
		t.Errorf("expected total 4, got %d", progress.TotalDone())
	}
	if progress.Total() != 4 {
		t.Errorf("expected total 4, got %d", progress.Total())
	}
}

func TestProgressRendersAllLanesToNonTerminalOutput(t *testing.T) {
	var buffer bytes.Buffer
	lanes := []*Lane{{Name: "metadata", Total: 2}, {Name: "lyrics", Total: 2}}
	progress := NewProgress(&buffer, lanes)
	progress.interval = time.Millisecond
	progress.started = time.Now().Add(-time.Minute)

	progress.Advance(lanes[0], OutcomeSucceeded, "")
	progress.Advance(lanes[1], OutcomeMissing, "")

	stop := progress.Start()
	stop()

	out := buffer.String()
	for _, want := range []string{"metadata", "lyrics", "total", "50.0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	// A non-terminal sink must never receive cursor-movement escapes.
	if strings.Contains(out, "\033[") {
		t.Errorf("non-terminal output must not contain escape codes, got:\n%q", out)
	}
}

func TestProgressConcurrentAdvanceIsSafe(t *testing.T) {
	const workers, advancesPerWorker = 4, 1000
	lane := &Lane{Name: "lane-1", Total: workers * advancesPerWorker}
	progress := NewProgress(&bytes.Buffer{}, []*Lane{lane})

	start := make(chan struct{})
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			defer func() { done <- struct{}{} }()
			for j := 0; j < advancesPerWorker; j++ {
				progress.Advance(lane, OutcomeSucceeded, "")
			}
		}()
	}
	// Read the counter concurrently too. Advance writes it under the renderer
	// mutex and Done reads it under the same one; use a ticker rather than
	// spinning so race coverage does not burn a CPU core.
	readerReady := make(chan struct{})
	readerStop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		close(readerReady)
		for {
			select {
			case <-ticker.C:
				_ = lane.Done()
			case <-readerStop:
				return
			}
		}
	}()
	<-readerReady
	close(start)
	for i := 0; i < workers; i++ {
		<-done
	}
	close(readerStop)
	<-readerDone
	if got, want := lane.Done(), int64(workers*advancesPerWorker); got != want {
		t.Fatalf("expected %d recorded advances, got %d", want, got)
	}
}

func TestFormatETA(t *testing.T) {
	const minute = time.Minute
	tests := []struct {
		name    string
		done    int64
		total   int64
		elapsed time.Duration
		want    string
	}{
		// No estimate is possible until a unit completes and time has passed.
		{"nothing done", 0, 100, time.Minute, "eta --"},
		{"no time yet", 5, 100, 0, "eta --"},
		// A streaming job has no known finish, so there is nothing to project.
		{"unknown total", 5, 0, time.Minute, "eta --"},
		// 1 unit/second, 950 left.
		{"seconds", 50, 1000, 50 * time.Second, "eta 15m50s"},
		// 1 unit/second, 3 left.
		{"under a minute", 97, 100, 97 * time.Second, "eta 3s"},
		// 1 unit/second, exactly 1 hour left. A whole hour drops the minutes.
		{"whole hours", 300, 3900, 300 * time.Second, "eta 1h"},
		// 2 units/second, 3800 left, so 31m40s: 1900s rounds down to 31m and the
		// remaining 40s is a multiple of ten.
		{"hours and minutes", 200, 4000, 100 * time.Second, "eta 31m40s"},
		// 2 units/second, 4800 left, so 40m exactly.
		{"long run", 200, 5000, 100 * time.Second, "eta 40m"},
		// 1 unit/second, 5400 left: the hour field is what an operator reads.
		{"over an hour", 600, 6000, 600 * time.Second, "eta 1h30m"},
		{"finished", 100, 100, time.Minute, "eta done"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatETA(tc.done, tc.total, tc.elapsed); got != tc.want {
				t.Errorf("formatETA(%d,%d,%s) = %q, want %q",
					tc.done, tc.total, tc.elapsed, got, tc.want)
			}
		})
	}
}

func TestFormatETADoesNotOverflowOnSlowRuns(t *testing.T) {
	// A run that took an hour to do two units is either stalled or throttled.
	// Projecting that rate onto 25,000 tracks yields a number that overflows a
	// duration when computed in nanoseconds, so the display must be capped
	// rather than print a wrapped or nonsensical figure.
	got := formatETA(2, 25_586, time.Hour)
	if !strings.HasPrefix(got, "eta ") {
		t.Fatalf("expected an eta prefix, got %q", got)
	}
	if strings.Contains(got, "-") || strings.Contains(got, "h-") {
		t.Errorf("expected a capped positive estimate, got %q", got)
	}
	if !strings.Contains(got, "100h") {
		t.Errorf("expected the cap to apply, got %q", got)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := map[time.Duration]string{
		45 * time.Second: "45s",
		// Seconds are truncated to a multiple of ten; 95s reads as 1m30s.
		95 * time.Second: "1m30s",
		89 * time.Second: "1m20s",
		// A whole minute and a whole hour both drop the smaller unit.
		120 * time.Second: "2m",
		2 * time.Hour:     "2h",
		25 * time.Hour:    "25h",
		90 * time.Minute:  "1h30m",
	}
	for input, want := range tests {
		if got := formatDuration(input); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", input, got, want)
		}
	}
}

func TestFrameShowsETA(t *testing.T) {
	var buffer bytes.Buffer
	lane := &Lane{Name: "lane-1", Total: 1000}
	progress := NewProgress(&buffer, []*Lane{lane})
	// A fixed start time keeps the estimate deterministic.
	progress.started = time.Now().Add(-100 * time.Second)
	progress.Advance(lane, OutcomeSucceeded, "")
	for i := 0; i < 99; i++ {
		progress.Advance(lane, OutcomeSucceeded, "")
	}
	progress.renderFinal()

	out := buffer.String()
	if !strings.Contains(out, "eta ") {
		t.Errorf("expected an eta in the frame, got:\n%s", out)
	}
}

func TestFrameOmitsETABeforeAnyWork(t *testing.T) {
	var buffer bytes.Buffer
	lane := &Lane{Name: "lane-1", Total: 1000}
	progress := NewProgress(&buffer, []*Lane{lane})
	progress.renderFinal()

	// The field is always present, but says it cannot estimate rather than
	// printing a wrong number.
	if !strings.Contains(buffer.String(), "eta --") {
		t.Errorf("expected an explicit unknown eta, got:\n%s", buffer.String())
	}
}

func TestNoticeReachesTheRenderedFrame(t *testing.T) {
	var buffer bytes.Buffer
	lane := &Lane{Name: "lane-1", Total: 1}
	progress := NewProgress(&buffer, []*Lane{lane})

	// Render directly instead of using Start/stop: the stop path sleeps for a
	// full redraw interval to let the renderer goroutine settle, which turns a
	// test that sets a long interval into a test that hangs.
	progress.Notice("lookup %q: %v", "Track - Artist", errors.New("boom"))
	progress.renderFinal()

	out := buffer.String()
	if !strings.Contains(out, `lookup "Track - Artist": boom`) {
		t.Errorf("expected the notice in the output, got:\n%s", out)
	}
	// A notice must never reach the output without its diagnostic prefix, since
	// that is what distinguishes it from a progress line.
	if strings.Count(out, "boom") != 1 {
		t.Errorf("expected exactly one copy of the message, got:\n%s", out)
	}
}

func TestNoticesAreCappedAndSuppressionIsCounted(t *testing.T) {
	var buffer bytes.Buffer
	lane := &Lane{Name: "lane-1", Total: 1}
	progress := NewProgress(&buffer, []*Lane{lane})

	// More than the retention limit, so the excess must be dropped rather than
	// growing without bound on a long run against a failing provider.
	for i := 0; i < maxNotices*4; i++ {
		progress.Notice("failure %d", i)
	}
	progress.renderFinal()

	out := buffer.String()
	// The retained tail includes the last few messages, in order. renderFinal
	// shows the newest maxReportedNotices of the retained window, so with 20
	// notices and a 5-line window the last three are 15, 16 and 17.
	for _, want := range []string{"failure 15", "failure 16", "failure 17"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the output, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "failure 0 ") {
		t.Errorf("expected old notices to be dropped, got:\n%s", out)
	}
	if !strings.Contains(out, "suppressed") {
		t.Errorf("expected dropped notices to be counted, got:\n%s", out)
	}
	// The frame itself must never grow without bound.
	printed := strings.Count(out, "\n")
	if printed > maxNotices+len(lane.Name)+8 {
		t.Errorf("expected a bounded frame, got %d lines:\n%s", printed, out)
	}
}

func TestWriteErrorsAppearInFrameOnlyWhenNonZero(t *testing.T) {
	var clean, failing bytes.Buffer
	lane := &Lane{Name: "lane-1", Total: 1}

	healthy := NewProgress(&clean, []*Lane{lane})
	healthy.renderFinal()
	if strings.Contains(clean.String(), "writes-failed") {
		t.Errorf("a healthy run must not show a write-error field, got:\n%s", clean.String())
	}

	broken := NewProgress(&failing, []*Lane{lane})
	broken.SetWriteErrors(7)
	broken.renderFinal()
	if !strings.Contains(failing.String(), "writes-failed 7") {
		t.Errorf("expected the failure count in the frame, got:\n%s", failing.String())
	}
}

func TestWriteFailedReflectsTheWriterState(t *testing.T) {
	run := &backfillRun{writeAbort: make(chan struct{}), maxWriteErrors: 4}
	if run.writeFailed() {
		t.Error("a fresh run must not report write failure")
	}
	run.noteWriteError(errors.New("readonly database"))
	if run.writeFailed() {
		t.Error("a single failed batch must not trip the abort")
	}
	// The threshold is the point at which the run gives up rather than
	// spending the rest of its upstream budget persisting nothing.
	for i := 0; i < 3; i++ {
		run.noteWriteError(errors.New("readonly database"))
	}
	if !run.writeFailed() {
		t.Error("expected the abort to trip at the threshold")
	}
	if run.abort() == nil {
		t.Error("expected an abort reason to be recorded")
	}
}

func TestWriteSuccessResetsTheFailureStreak(t *testing.T) {
	run := &backfillRun{writeAbort: make(chan struct{}), writeOK: make(chan struct{}, 1), maxWriteErrors: 5}
	// Four failures fall short of the threshold on their own.
	for i := 0; i < 4; i++ {
		run.noteWriteError(errors.New("database is locked"))
	}
	if run.writeFailed() {
		t.Fatal("four failures must not trip a threshold of five")
	}
	// A busy database that recovers must not accumulate failures across the
	// whole run and trip an abort on a stale streak: the success clears it, so
	// the next single failure starts a new count rather than continuing one.
	run.noteWriteSuccess()
	run.noteWriteError(errors.New("database is locked"))
	if run.writeFailed() {
		t.Error("expected a recovered streak not to trip the abort")
	}
}

// The run header is the only place an operator can see the rate the job is
// actually held to, so it has to be right in the direction that matters: never
// claiming more throughput than the pace allows.
func TestPaceTextStatesTheRealRate(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     string
	}{
		{"one second reads as a rate", time.Second, "1 req/s"},
		{"two seconds is half a request, not one", 2 * time.Second, "1 req/2s"},
		{"ten seconds", 10 * time.Second, "1 req/10s"},
		{"a minute", time.Minute, "1 req/60s"},
		{"fast providers keep the whole req/s form", 200 * time.Millisecond, "5 req/s"},
		{"lyricsplus", 300 * time.Millisecond, "4 req/s"},
		{"not a whole number of seconds", 1500 * time.Millisecond, "1 req/1.5s"},
		{"no pacing configured", 0, "unpaced"},
		{"a negative interval is not a rate", -time.Second, "unpaced"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := paceText(test.interval); got != test.want {
				t.Fatalf("paceText(%s) = %q, want %q", test.interval, got, test.want)
			}
		})
	}
}

// Rounding a slow pace up to a whole req/s would overstate it, which is the one
// error a rate line cannot make: an operator reading "1 req/s" from a run paced at
// one request every two seconds is being told the run may move twice as fast as it
// may.
func TestPaceTextNeverOverstatesASlowPace(t *testing.T) {
	for _, interval := range []time.Duration{2 * time.Second, 3 * time.Second, 4 * time.Second, 30 * time.Second} {
		got := paceText(interval)
		if strings.Contains(got, "req/s") {
			t.Fatalf("paceText(%s) = %q, which claims a whole request per second the run cannot make", interval, got)
		}
	}
}
