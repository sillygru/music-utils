package db

// Track is the database representation of a song's metadata.
type Track struct {
	ID                        int64
	Name                      string
	NameLower                 string
	ArtistName                string
	ArtistNameLower           string
	AlbumName                 string
	AlbumNameLower            string
	Duration                  float64
	Genre                     string
	GenreLower                string
	Year                      int
	ReleaseDate               string
	ISRC                      string
	MusicBrainzRecordingID    string
	MusicBrainzReleaseID      string
	MusicBrainzReleaseGroupID string
	MusicBrainzArtistID       string
	CoverURL                  string
	MetadataSource            string
	CoverURLSource            string
	MetadataChecked           bool
	CoverURLChecked           bool
	LastLyricsID              int64
	Source                    string
}

// CoverEntity distinguishes album art from artist art in the cover_urls table.
type CoverEntity string

const (
	CoverArtist CoverEntity = "artist"
	CoverAlbum  CoverEntity = "album"
)

// CoverArt is the database representation of a cached album or artist cover URL.
type CoverArt struct {
	ID              int64
	EntityType      CoverEntity
	ArtistNameLower string
	AlbumNameLower  string
	CoverURL        string
	CoverSource     string
	CheckedAt       string
}

// Lyrics is the database representation of a track's lyrics. TrackID records
// the original owning track; lyrics_tracks contains every track association
// when content deduplication shares this row.
type Lyrics struct {
	ID           int64
	TrackID      int64
	PlainLyrics  string
	SyncedLyrics string
	HasPlain     bool
	HasSynced    bool
	Instrumental bool
	ContentHash  string
	Source       string
}
