package httpserver

import (
	"strconv"
	"strings"
)

const lyricsCachePolicyVersion = "v2"

// lyricsProviderVariant identifies an independently cacheable representation.
type lyricsProviderVariant struct {
	Provider string
	SyncType string
	Format   string
}

func canonicalLyricsKey(track, artist, album string, duration float64, includeRich bool, syncType string) string {
	return lyricsCachePolicyVersion + "\x00" + canonicalPart(track) + "\x00" + canonicalPart(artist) + "\x00" + canonicalPart(album) + "\x00" + canonicalPart(formatDuration(duration)) + "\x00" + canonicalPart(cacheBoolString(includeRich)) + "\x00" + canonicalPart(syncType)
}

func canonicalPart(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}
func cacheBoolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
func formatDuration(value float64) string {
	if value <= 0 {
		return ""
	}
	return strings.TrimRight(strings.TrimRight(fmtFloat(value), "0"), ".")
}
func fmtFloat(value float64) string { return strconv.FormatFloat(value, 'f', 3, 64) }

func providerVariantKey(base, provider, syncType, format string) string {
	return base + "\x00" + canonicalPart(provider) + "\x00" + canonicalPart(syncType) + "\x00" + canonicalPart(format)
}
