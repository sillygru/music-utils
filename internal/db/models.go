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
	// LyricsChecked records that a lyrics provider gave a definitive answer for
	// the track, whether that answer was lyrics or a confirmed miss. It is the
	// lyrics counterpart of MetadataChecked and is driven the same way: set on a
	// real result, left alone by a write that carries no upstream answer.
	//
	// It is not read back from the database, because nothing needs to know an
	// individual track's settle state; the jobs select whole sets of them. It
	// exists to be written and to be counted.
	LyricsChecked   bool
	CoverURLChecked bool
	LastLyricsID    int64
	Source          string
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

// CoverVariant is one alternative cover URL for an album or artist. Rank 0 is
// the winner mirrored on the parent cover_urls row; higher ranks are the other
// plausible provider URLs in provider order.
type CoverVariant struct {
	URL    string
	Source string
	Rank   int
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

// RichLyrics is a cached source-native word or syllable synchronized payload.
type RichLyrics struct {
	ID       int64
	TrackID  int64
	Content  string
	Format   string
	SyncType string
	Source   string
	Hash     string
}
