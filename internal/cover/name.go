package cover

import (
	"strings"

	"github.com/sillygru/music-utils/internal/names"
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
func CleanArtist(value string) string { return cleanTag(names.CleanArtist(value)) }

// CleanAlbum returns a non-empty album name only when it is worth querying.
func CleanAlbum(value string) string { return cleanTag(names.CleanAlbum(value)) }

func normalizeInput(input Input) Input {
	cleaned := names.Normalize(input.TrackName, input.ArtistName, input.AlbumName)
	return Input{TrackName: cleaned.TrackName, ArtistName: cleaned.ArtistName, AlbumName: cleaned.AlbumName}
}

func candidateInputs(input Input) []Input {
	candidates := names.Candidates(input.TrackName, input.ArtistName, input.AlbumName)
	result := make([]Input, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, Input{TrackName: candidate.TrackName, ArtistName: candidate.ArtistName, AlbumName: candidate.AlbumName})
	}
	return result
}

func songResultMatches(input Input, result Result) bool {
	if input.TrackName != "" && result.TrackName != "" {
		track, candidate := normalize(input.TrackName), normalize(result.TrackName)
		if track != candidate && !strings.Contains(track, candidate) && !strings.Contains(candidate, track) {
			return false
		}
	}
	if input.ArtistName != "" && result.ArtistName != "" {
		artist, candidate := normalize(input.ArtistName), normalize(result.ArtistName)
		if artist != candidate && !strings.Contains(artist, candidate) && !strings.Contains(candidate, artist) {
			return false
		}
	}
	return true
}
