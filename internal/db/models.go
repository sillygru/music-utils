package db

// Track is the database representation of a song's metadata.
type Track struct {
	ID              int64
	Name            string
	NameLower       string
	ArtistName      string
	ArtistNameLower string
	AlbumName       string
	AlbumNameLower  string
	Duration        float64
	LastLyricsID    int64
	Source          string
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
