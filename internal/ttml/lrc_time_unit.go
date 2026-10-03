package ttml

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxPlausibleSyncedSeconds bounds the runtime of an ordinary recording. A tag
// beyond this is not a long song, it is a timing in the wrong unit.
const MaxPlausibleSyncedSeconds = 3600

// lrcTagRun matches the run of timestamp tags that opens an LRC line. The minute
// field is deliberately unbounded: lyricsfile.go's lrcLinePattern accepts only
// one to three digits, so a provider that writes milliseconds through a
// seconds-based formatter emits four- and five-digit minute fields, and those are
// precisely the tags this has to recognise in order to repair them.
var lrcTagRun = regexp.MustCompile(`^(?:\[\d+:\d{1,2}(?:[.:]\d{1,3})?\])+`)

// lrcTag parses one tag: minutes, seconds, and an optional fraction that is
// milliseconds by digit count, matching lrcTimestampMS.
var lrcTag = regexp.MustCompile(`\[(\d+):(\d{1,2})(?:[.:](\d{1,3}))?\]`)

// corruptLrcMinuteFloor is the smallest minute field a millisecond value can
// produce for an ordinary recording: 60s is 60000ms, which formats as minute
// 1000. Any minute field at or above this is therefore a unit error rather than
// a long song, and dividing by 1000 recovers the intended time.
const corruptLrcMinuteFloor = 1000

// NormalizeSyncedTimeUnit rescales LRC timings that cannot plausibly be seconds,
// so a provider that forgets to convert is corrected on the way out instead of
// shipping timings that never line up with playback.
//
// It is the line-synced counterpart to httpserver.normalizeRichSyncTimeUnit. That
// one works from a parsed payload, where millisecond scale shows up as absurd
// per-word and per-line durations; a formatted LRC string carries no durations,
// only absolute stamps, so the giveaway is the width of the minute field.
//
// Only tags inside a line's leading tag run are considered, so a bracketed
// number in lyric text is never touched. maxPlausibleSeconds bounds the repair:
// if dividing does not land inside a plausible recording the original is
// returned unchanged, because a genuinely long recording must survive intact.
// The boolean reports whether s was rewritten.
func NormalizeSyncedTimeUnit(s string, maxPlausibleSeconds float64) (string, bool) {
	if s == "" {
		return "", false
	}
	lines := strings.Split(s, "\n")
	rewrote := false
	for i, line := range lines {
		run := lrcTagRun.FindString(line)
		if run == "" {
			continue
		}
		repaired, changed := repairLRCTagRun(run)
		if !changed {
			continue
		}
		lines[i] = repaired + line[len(run):]
		rewrote = true
	}
	if !rewrote {
		return s, false
	}
	repaired := strings.Join(lines, "\n")
	if LastLRCTagSeconds(repaired) > maxPlausibleSeconds {
		return s, false
	}
	return repaired, true
}

// repairLRCTagRun divides every millisecond-scale tag in a leading tag run,
// leaving sub-minute tags exactly as they were.
func repairLRCTagRun(run string) (string, bool) {
	matches := lrcTag.FindAllStringIndex(run, -1)
	if len(matches) == 0 {
		return run, false
	}
	var b strings.Builder
	changed := false
	prev := 0
	for _, loc := range matches {
		tag := run[loc[0]:loc[1]]
		b.WriteString(run[prev:loc[0]])
		repaired, ok := repairLRCTag(tag)
		if ok {
			changed = true
			b.WriteString(repaired)
		} else {
			b.WriteString(tag)
		}
		prev = loc[1]
	}
	b.WriteString(run[prev:])
	return b.String(), changed
}

// repairLRCTag rescales one tag, reporting false for a tag that is already in
// seconds or is malformed beyond a safe reading.
func repairLRCTag(tag string) (string, bool) {
	m := lrcTag.FindStringSubmatch(tag)
	if m == nil {
		return tag, false
	}
	minutes, err := strconv.Atoi(m[1])
	if err != nil || minutes < corruptLrcMinuteFloor {
		return tag, false
	}
	seconds, err := strconv.Atoi(m[2])
	if err != nil || seconds < 0 || seconds > 59 {
		return tag, false
	}
	totalMS := minutes*60*1000 + seconds*1000
	if m[3] != "" {
		frac, err := strconv.Atoi(m[3])
		if err != nil {
			return tag, false
		}
		for i := len(m[3]); i < 3; i++ {
			frac *= 10
		}
		totalMS += frac
	}
	totalMS /= 1000
	whole, rest := totalMS/60000, totalMS%60000
	return fmt.Sprintf("[%02d:%02d.%02d]", whole, rest/1000, rest%1000/10), true
}

// LastLRCTagSeconds is the largest timing anywhere in the string, in seconds. It
// is exported so a caller that rescales a string can report where it landed.
func LastLRCTagSeconds(s string) float64 {
	var maxMS int
	for _, m := range lrcTag.FindAllStringSubmatch(s, -1) {
		minutes, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		seconds, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		totalMS := minutes*60*1000 + seconds*1000
		if m[3] != "" {
			frac, err := strconv.Atoi(m[3])
			if err != nil {
				continue
			}
			for i := len(m[3]); i < 3; i++ {
				frac *= 10
			}
			totalMS += frac
		}
		if totalMS > maxMS {
			maxMS = totalMS
		}
	}
	return float64(maxMS) / 1000
}
