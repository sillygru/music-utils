package httpserver

import (
	"github.com/sillygru/music-utils/internal/ttml"
)

// normalizeStoredSyncedLyrics cleans an LRC string read back out of the database
// and repairs timings that cannot plausibly be seconds.
//
// Providers declare their upstream unit and convert at the boundary, and the
// rich payload has normalizeRichSyncTimeUnit as a read-side safety net. The
// line-synced string had no equivalent, so a row stored by a provider that
// forgot the conversion shipped to clients with every stamp past the first
// minute inflated a thousandfold. This is that missing guard; it reports rather
// than silently corrects, so the provider bug that caused it stays visible.
func normalizeStoredSyncedLyrics(synced string, source string) string {
	cleaned := ttml.CleanSyncedLyrics(synced)
	repaired, changed := ttml.NormalizeSyncedTimeUnit(cleaned, maxPlausibleLyricsSeconds)
	if changed {
		getLyricsTimeUnitLogger().Warn("synced lyrics timings were not in seconds, rescaled from milliseconds",
			"source", source, "last_tag_seconds", ttml.LastLRCTagSeconds(repaired))
	}
	return repaired
}
