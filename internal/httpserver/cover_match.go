package httpserver

import (
	"strings"

	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
)

// coverNormalize lowercases and trims a name for cover matching.
func coverNormalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

// coverTokenSet returns the set of whitespace-separated tokens in a name.
func coverTokenSet(value string) map[string]bool {
	tokens := make(map[string]bool)
	for _, token := range strings.Fields(coverNormalize(value)) {
		tokens[token] = true
	}
	return tokens
}

// coverTokensCover reports whether every token of want appears in candidate.
// An empty want matches nothing.
func coverTokensCover(want, candidate string) bool {
	wantTokens := coverTokenSet(want)
	if len(wantTokens) == 0 {
		return false
	}
	candidateTokens := coverTokenSet(candidate)
	for token := range wantTokens {
		if !candidateTokens[token] {
			return false
		}
	}
	return true
}

// coverNameSimilar reports whether two names are equal or one contains the
// other after normalization. Empty inputs are never similar.
func coverNameSimilar(a, b string) bool {
	a, b = coverNormalize(a), coverNormalize(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.Contains(a, b) || strings.Contains(b, a)
}

// coverArtistSame reports whether two artist names refer to the same artist:
// equal token sets after ignoring a leading "the". Unlike coverNameSimilar it
// never accepts a bare substring match, so "Wings On Eagles" is not the same
// artist as "Eagles" and unrelated names sharing a word do not slip through.
func coverArtistSame(a, b string) bool {
	a = strings.TrimPrefix(coverNormalize(a), "the ")
	b = strings.TrimPrefix(coverNormalize(b), "the ")
	at, bt := coverTokenSet(a), coverTokenSet(b)
	if len(at) == 0 || len(bt) == 0 {
		return false
	}
	for token := range at {
		if !bt[token] {
			return false
		}
	}
	for token := range bt {
		if !at[token] {
			return false
		}
	}
	return true
}

// coverArtistsShareToken reports whether two artist names have any token in
// common after ignoring a leading "the". A result artist that shares a word
// with the requested artist is a near-miss that only looks plausible (for
// example "Wings On Eagles" for "Eagles"); it must never be treated as the
// compilation-artist case, where the credited artist shares nothing.
func coverArtistsShareToken(a, b string) bool {
	a = strings.TrimPrefix(coverNormalize(a), "the ")
	b = strings.TrimPrefix(coverNormalize(b), "the ")
	at, bt := coverTokenSet(a), coverTokenSet(b)
	for token := range at {
		if bt[token] {
			return true
		}
	}
	return false
}

// coverResultMatches reports whether a provider result plausibly corresponds to
// the requested entity. Artist results must share the artist name. Album
// results must cover every requested album token; an artist mismatch is
// tolerated only when the album name matches exactly and the credited artist
// does not merely resemble the requested one (soundtracks and various-artist
// releases credit a different entity entirely). Song results are not filtered
// because track matching is intentionally fuzzy.
func coverResultMatches(kind cover.Kind, input cover.Input, result cover.Result) bool {
	switch kind {
	case cover.Artist:
		return coverNameSimilar(input.ArtistName, result.ArtistName)
	case cover.Album:
		if !coverTokensCover(input.AlbumName, result.AlbumName) {
			return false
		}
		if coverArtistSame(input.ArtistName, result.ArtistName) {
			return true
		}
		if coverArtistsShareToken(input.ArtistName, result.ArtistName) {
			return false
		}
		return coverNormalize(input.AlbumName) == coverNormalize(result.AlbumName)
	default:
		return true
	}
}

// filterCoverResults drops provider results that do not plausibly match the
// requested entity. The returned slice shares no backing array with results.
func filterCoverResults(kind cover.Kind, input cover.Input, results []cover.Result) []cover.Result {
	filtered := make([]cover.Result, 0, len(results))
	for _, result := range results {
		if coverResultMatches(kind, input, result) {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

// coverEntityForKind maps a cover kind to its cover-url table entity.
func coverEntityForKind(kind cover.Kind) db.CoverEntity {
	if kind == cover.Album {
		return db.CoverAlbum
	}
	return db.CoverArtist
}
