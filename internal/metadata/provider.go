package metadata

import (
	"context"
	"errors"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/names"
)

// ErrNotFound is returned when no provider can resolve a track.
var ErrNotFound = errors.New("track not found in any metadata provider")

// ErrInconclusive reports that a lookup failed for a reason unrelated to the
// song: a provider error, a timeout, or a network failure.
//
// It is deliberately distinct from ErrNotFound, which asserts the song
// genuinely has no upstream match. A lookup that is merely inconclusive must
// never be persisted as a miss, or a momentary network blip would permanently
// record a song as having no data and no later run would retry it.
var ErrInconclusive = errors.New("metadata lookup was inconclusive")

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
