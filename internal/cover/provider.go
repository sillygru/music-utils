// Package cover resolves album and artist cover artwork URLs from three
// keyless sources in a fixed fallback order (Last.fm, iTunes, then Deezer) and
// returns the first non-empty URL. Only URLs are produced; image bytes are out
// of scope. Persistence to a dedicated SQLite database lives in the db package
// and is driven by the HTTP handlers, not here.
package cover

import (
	"context"
	"time"
)

// Provider resolves album or artist artwork from a single upstream source.
type Provider interface {
	Name() string
	Lookup(ctx context.Context, kind Kind, input Input) (*Result, error)
}

// Kind selects which artwork is being looked up.
type Kind int

const (
	// Artist requests artist artwork.
	Artist Kind = iota
	// Album requests album artwork.
	Album
)

func (k Kind) String() string {
	if k == Album {
		return "album"
	}
	return "artist"
}

// Input is the normalized identity used to look up artwork.
type Input struct {
	ArtistName string
	AlbumName  string
}

// Result is a resolved cover URL and the source that produced it.
type Result struct {
	URL    string
	Source string
}

// Name cleanup TTL linked to the negative-cache expiry used by callers. The
// resolver itself does not persist, so this constant documents the intended
// expiry for handlers that read CheckedAt.
const NegativeCacheTTL = 24 * time.Hour
