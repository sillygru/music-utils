package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// FindTrackExact returns the matching track and its latest lyrics entry.
func FindTrackExact(ctx context.Context, database *sql.DB, name, artist, album string, duration float64) (*Track, *Lyrics, error) {
	if database == nil {
		return nil, nil, errors.New("database is nil")
	}

	trackName := normalize(name)
	artistName := normalize(artist)
	albumName := normalize(album)
	query := `
SELECT
    t.id, t.name, t.name_lower, t.artist_name, t.artist_name_lower,
    COALESCE(t.album_name, ''), COALESCE(t.album_name_lower, ''), COALESCE(t.duration, 0),
    COALESCE(t.last_lyrics_id, 0), t.source,
    l.id, l.track_id, COALESCE(l.plain_lyrics, ''), COALESCE(l.synced_lyrics, ''),
    l.has_plain_lyrics, l.has_synced_lyrics, l.instrumental,
    l.content_hash, l.source
FROM tracks AS t
JOIN lyrics AS l ON l.id = t.last_lyrics_id
WHERE t.name_lower = ?
  AND t.artist_name_lower = ?`
	args := []any{trackName, artistName}
	if albumName != "" {
		query += "\n  AND t.album_name_lower = ?"
		args = append(args, albumName)
	}
	if duration > 0 {
		query += "\n  AND t.duration = ?"
		args = append(args, duration)
	}
	query += "\nLIMIT 1"

	row := database.QueryRowContext(ctx, query, args...)

	track, lyrics, err := scanTrackAndLyrics(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, nil, fmt.Errorf("find exact track: %w", err)
	}
	return track, lyrics, nil
}

// FindLyricsByID returns one lyrics record by its database ID.
func FindLyricsByID(ctx context.Context, database *sql.DB, lyricsID int64) (*Lyrics, error) {
	if database == nil {
		return nil, errors.New("database is nil")
	}

	const query = `
SELECT id, track_id, COALESCE(plain_lyrics, ''), COALESCE(synced_lyrics, ''),
       has_plain_lyrics, has_synced_lyrics, instrumental, content_hash, source
FROM lyrics
WHERE id = ?
LIMIT 1`

	lyrics := &Lyrics{}
	err := database.QueryRowContext(ctx, query, lyricsID).Scan(
		&lyrics.ID, &lyrics.TrackID, &lyrics.PlainLyrics, &lyrics.SyncedLyrics,
		&lyrics.HasPlain, &lyrics.HasSynced, &lyrics.Instrumental,
		&lyrics.ContentHash, &lyrics.Source,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("find lyrics by id: %w", err)
	}
	return lyrics, nil
}

// TrackSearchResult contains a track and its latest lyrics row from one query.
type TrackSearchResult struct {
	Track
	Lyrics
}

// SearchTracks searches normalized track metadata and lyrics through the FTS5 index.
func SearchTracks(ctx context.Context, database *sql.DB, query string, limit int) ([]TrackSearchResult, error) {
	if database == nil {
		return nil, errors.New("database is nil")
	}
	if limit < 1 {
		return []TrackSearchResult{}, nil
	}
	if limit > 100 {
		limit = 100
	}

	const statement = `
SELECT t.id, t.name, t.name_lower, t.artist_name, t.artist_name_lower,
       COALESCE(t.album_name, ''), COALESCE(t.album_name_lower, ''), COALESCE(t.duration, 0),
       COALESCE(t.last_lyrics_id, 0), t.source,
       COALESCE(l.id, 0), COALESCE(l.track_id, 0),
       COALESCE(l.plain_lyrics, ''), COALESCE(l.synced_lyrics, ''),
       COALESCE(l.has_plain_lyrics, 0), COALESCE(l.has_synced_lyrics, 0),
       COALESCE(l.instrumental, 0), COALESCE(l.content_hash, ''), COALESCE(l.source, '')
FROM tracks_fts AS f
JOIN tracks AS t ON t.id = f.rowid
LEFT JOIN lyrics AS l ON l.id = t.last_lyrics_id
WHERE tracks_fts MATCH ?
ORDER BY t.id
LIMIT ?`

	match := ftsQuery(query)
	if match == "" {
		return []TrackSearchResult{}, nil
	}

	rows, err := database.QueryContext(ctx, statement, match, limit)
	if err != nil {
		return nil, fmt.Errorf("search tracks: %w", err)
	}
	defer rows.Close()

	tracks := make([]TrackSearchResult, 0, limit)
	for rows.Next() {
		track, lyrics, err := scanTrackAndLyrics(rows)
		if err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		tracks = append(tracks, TrackSearchResult{Track: *track, Lyrics: *lyrics})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search results: %w", err)
	}
	return tracks, nil
}

// InsertTrackWithLyrics inserts or reuses a track and deduplicated lyrics
// record in one transaction, then points the track at that lyrics record.
func InsertTrackWithLyrics(ctx context.Context, database *sql.DB, track Track, lyrics Lyrics) (trackID, lyricsID int64, err error) {
	if database == nil {
		return 0, 0, errors.New("database is nil")
	}

	track.Name = strings.TrimSpace(track.Name)
	track.ArtistName = strings.TrimSpace(track.ArtistName)
	track.AlbumName = strings.TrimSpace(track.AlbumName)
	track.NameLower = normalize(track.Name)
	track.ArtistNameLower = normalize(track.ArtistName)
	track.AlbumNameLower = normalize(track.AlbumName)
	if track.Source == "" {
		track.Source = "local"
	}
	if lyrics.Source == "" {
		lyrics.Source = track.Source
	}
	if lyrics.ContentHash == "" {
		lyrics.ContentHash = contentHash(lyrics.PlainLyrics, lyrics.SyncedLyrics)
	}
	lyrics.HasPlain = lyrics.HasPlain || lyrics.PlainLyrics != ""
	lyrics.HasSynced = lyrics.HasSynced || lyrics.SyncedLyrics != ""

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin lyrics insert: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	const insertTrack = `
INSERT INTO tracks (
    name, name_lower, artist_name, artist_name_lower,
    album_name, album_name_lower, duration, source
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name_lower, artist_name_lower, album_name_lower, duration)
DO UPDATE SET updated_at = CURRENT_TIMESTAMP, source = excluded.source
RETURNING id`
	if err = tx.QueryRowContext(ctx, insertTrack,
		track.Name, track.NameLower, track.ArtistName, track.ArtistNameLower,
		track.AlbumName, track.AlbumNameLower, track.Duration, track.Source,
	).Scan(&trackID); err != nil {
		return 0, 0, fmt.Errorf("insert track: %w", err)
	}

	const insertLyrics = `
INSERT OR IGNORE INTO lyrics (
    track_id, plain_lyrics, synced_lyrics, has_plain_lyrics,
    has_synced_lyrics, instrumental, content_hash, source
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id`
	if err = tx.QueryRowContext(ctx, insertLyrics,
		trackID, nullableText(lyrics.PlainLyrics), nullableText(lyrics.SyncedLyrics),
		lyrics.HasPlain, lyrics.HasSynced, lyrics.Instrumental,
		lyrics.ContentHash, lyrics.Source,
	).Scan(&lyricsID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, fmt.Errorf("insert lyrics: %w", err)
		}
		const selectLyricsID = `SELECT id FROM lyrics WHERE content_hash = ? LIMIT 1`
		if err = tx.QueryRowContext(ctx, selectLyricsID, lyrics.ContentHash).Scan(&lyricsID); err != nil {
			return 0, 0, fmt.Errorf("select lyrics id: %w", err)
		}
	}

	const associateLyrics = `INSERT OR IGNORE INTO lyrics_tracks (track_id, lyrics_id) VALUES (?, ?)`
	if _, err = tx.ExecContext(ctx, associateLyrics, trackID, lyricsID); err != nil {
		return 0, 0, fmt.Errorf("associate lyrics with track: %w", err)
	}

	const updateTrack = `UPDATE tracks SET last_lyrics_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	if _, err = tx.ExecContext(ctx, updateTrack, lyricsID, trackID); err != nil {
		return 0, 0, fmt.Errorf("link lyrics to track: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit lyrics insert: %w", err)
	}
	return trackID, lyricsID, nil
}

func scanTrackAndLyrics(scanner interface{ Scan(...any) error }) (*Track, *Lyrics, error) {
	track := &Track{}
	lyrics := &Lyrics{}
	err := scanner.Scan(
		&track.ID, &track.Name, &track.NameLower, &track.ArtistName,
		&track.ArtistNameLower, &track.AlbumName, &track.AlbumNameLower,
		&track.Duration, &track.LastLyricsID, &track.Source,
		&lyrics.ID, &lyrics.TrackID, &lyrics.PlainLyrics, &lyrics.SyncedLyrics,
		&lyrics.HasPlain, &lyrics.HasSynced, &lyrics.Instrumental,
		&lyrics.ContentHash, &lyrics.Source,
	)
	return track, lyrics, err
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func ftsQuery(value string) string {
	parts := strings.Fields(normalize(value))
	for i, part := range parts {
		parts[i] = `"` + strings.ReplaceAll(part, `"`, `""`) + `"*`
	}
	return strings.Join(parts, " AND ")
}

func contentHash(plain, synced string) string {
	hash := sha256.Sum256([]byte(normalize(plain) + "\x00" + normalize(synced)))
	return hex.EncodeToString(hash[:])
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
