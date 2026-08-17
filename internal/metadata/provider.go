package metadata

import (
	"context"
	"errors"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/names"
)

// ErrNotFound is returned when no provider can resolve a track.
var ErrNotFound = errors.New("track not found in any metadata provider")

// Input is the normalized identity used to look up a track.
type Input struct {
	TrackName  string
	ArtistName string
	AlbumName  string
	Duration   float64
}

func normalizeInput(input Input) Input {
	cleaned := names.Normalize(input.TrackName, input.ArtistName, input.AlbumName)
	return Input{TrackName: cleaned.TrackName, ArtistName: cleaned.ArtistName, AlbumName: cleaned.AlbumName, Duration: input.Duration}
}

func inputCandidates(input Input) []Input {
	candidates := names.Candidates(input.TrackName, input.ArtistName, input.AlbumName)
	result := make([]Input, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, Input{TrackName: candidate.TrackName, ArtistName: candidate.ArtistName, AlbumName: candidate.AlbumName, Duration: input.Duration})
	}
	return result
}

// Provider resolves track metadata from a single upstream source.
type Provider interface {
	Name() string
	Lookup(ctx context.Context, input Input) (*db.Track, error)
}

// SearchProvider returns multiple metadata matches from a single upstream
// source. It is intentionally separate from Provider so small test providers
// and future lookup-only integrations remain valid.
type SearchProvider interface {
	Search(ctx context.Context, query string, limit int) ([]*db.Track, error)
}
