package httpserver

import (
	"time"

	"github.com/sillygru/music-utils/internal/cover"
	"github.com/sillygru/music-utils/internal/db"
)

const lastFMTimeFormat = "2006-01-02 15:04:05"

// albumArtistCoverResponse is the shared response shape for the artist and
// album cover routes: the legacy top-level fields remain available, while
// Results exposes every configured provider result.
type albumArtistCoverResponse struct {
	ID          int64                 `json:"id"`
	EntityType  string                `json:"entityType"`
	ArtistName  string                `json:"artistName,omitempty"`
	AlbumName   string                `json:"albumName,omitempty"`
	CoverURL    string                `json:"coverUrl,omitempty"`
	CoverSource string                `json:"coverUrlSource,omitempty"`
	Results     []coverSearchResponse `json:"results,omitempty"`
}

func toKind(entityType db.CoverEntity) cover.Kind {
	if entityType == db.CoverAlbum {
		return cover.Album
	}
	return cover.Artist
}

func checkedRecently(checkedAt string) bool {
	if checkedAt == "" {
		return false
	}
	checked, err := time.Parse(lastFMTimeFormat, checkedAt)
	if err != nil {
		return false
	}
	return time.Since(checked) < cover.NegativeCacheTTL
}

func albumArtistCoverFromRow(row *db.CoverArt, entityType db.CoverEntity, artist, album string) albumArtistCoverResponse {
	return albumArtistCoverResponse{
		ID: row.ID, EntityType: string(entityType), ArtistName: artist, AlbumName: album,
		CoverURL: row.CoverURL, CoverSource: row.CoverSource,
	}
}
