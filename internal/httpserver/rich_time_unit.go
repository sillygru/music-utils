package httpserver

import (
	"log/slog"
	"sync/atomic"

	"github.com/sillygru/music-utils/internal/ttml"
)

// maxPlausibleLyricsSeconds bounds the runtime of an ordinary recording.
const maxPlausibleLyricsSeconds = 3600

var lyricsTimeUnitLogger atomic.Pointer[slog.Logger]

func setLyricsTimeUnitLogger(logger *slog.Logger) {
	if logger != nil {
		lyricsTimeUnitLogger.Store(logger)
	}
}

func getLyricsTimeUnitLogger() *slog.Logger {
	if l := lyricsTimeUnitLogger.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// normalizeRichSyncTimeUnit rescales a parsed rich payload whose timings cannot
// plausibly be seconds. Providers declare their upstream unit at the boundary
// and convert with ttml.MillisToSeconds; this is the read-side counterpart, so
// a provider that forgets is corrected here and reported, instead of shipping
// timings that never line up with playback.
func normalizeRichSyncTimeUnit(parsed compactRichSync, source string) compactRichSync {
	maxEnd := parsed.Duration
	hasMilliScale := false
	for _, line := range parsed.Lines {
		if line.End > maxEnd {
			maxEnd = line.End
		}
		if (line.End - line.Begin) > 120 {
			hasMilliScale = true
		}
		for _, w := range line.Words {
			if (w.End - w.Begin) > 30 {
				hasMilliScale = true
			}
		}
	}
	// A payload is only rescaled if its maximum timing exceeds plausible recording
	// length and either exhibits millisecond-scale element durations (>30s per word or
	// >120s per line) or is astronomically large (>24h). This prevents misclassifying
	// legitimate long progressive rock, classical, or live mix recordings in seconds.
	if maxEnd <= maxPlausibleLyricsSeconds || (!hasMilliScale && maxEnd <= 86400 && len(parsed.Lines) > 0) {
		return parsed
	}
	for i := range parsed.Lines {
		parsed.Lines[i].Begin = ttml.MillisToSeconds(parsed.Lines[i].Begin)
		parsed.Lines[i].End = ttml.MillisToSeconds(parsed.Lines[i].End)
		for j := range parsed.Lines[i].Words {
			parsed.Lines[i].Words[j].Begin = ttml.MillisToSeconds(parsed.Lines[i].Words[j].Begin)
			parsed.Lines[i].Words[j].End = ttml.MillisToSeconds(parsed.Lines[i].Words[j].End)
		}
	}
	if parsed.Duration > 0 {
		parsed.Duration = ttml.MillisToSeconds(parsed.Duration)
	} else {
		parsed.Duration = ttml.MillisToSeconds(maxEnd)
	}
	getLyricsTimeUnitLogger().Warn("rich lyrics timings were not in seconds, rescaled from milliseconds",
		"source", source, "rescaled_end", parsed.Duration)
	return parsed
}
