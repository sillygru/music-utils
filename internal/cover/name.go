package cover

import (
	"strings"
)

// cleanTag trims a name and returns "" for the sentinel "unknown" values so
// they are never sent as upstream queries. Album/artist art is not searched
// for tags that normalize to a sentinel.
func cleanTag(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "unknown", "unknown artist", "unknown album", "unknown title", "":
		return ""
	}
	return value
}

// normalize lowercases and trims a name for matching and cleaning.
func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

// CleanArtist returns a non-empty artist name only when it is worth querying.
func CleanArtist(value string) string { return cleanTag(value) }

// CleanAlbum returns a non-empty album name only when it is worth querying.
func CleanAlbum(value string) string { return cleanTag(value) }
