package metadata

import (
	"context"
	"errors"

	"github.com/sillygru/music-utils/internal/db"
)

// ErrNotFound is returned when no provider can resolve a track.
var ErrNotFound = errors.New("track not found in any metadata provider")

// Input is the normalized identity used to look up a track.
type Input struct {
	TrackName string
	ArtistName string
	AlbumName  string
	Duration   float64
}

// Provider resolves track metadata from a single upstream source.
type Provider interface {
	Name() string
	Lookup(ctx context.Context, input Input) (*db.Track, error)
}