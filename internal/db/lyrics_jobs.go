package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LyricsWork is one track awaiting an upstream lyrics lookup, reduced to the
// fields needed to build a provider query plus the row identity used to write
// the result back.
//
// ISRC is carried because one provider (LyricsPlus) uses it to match a recording
// more precisely than the title and artist alone, and the metadata backfill has
// no equivalent reason to keep it.
type LyricsWork struct {
	ID       int64
	Name     string
	Artist   string
	Album    string
	Duration float64
	ISRC     string
	// CheckedAt is the stored settle time for a stale refresh candidate, in the
	// same form the column holds, or empty when the answer predates the column.
	CheckedAt string
}

// lyricsStaleAge is the lyrics counterpart of the metadata stale-age expression.
//
// It is the same shape and for the same reasons: mapping an unknown settle time
// to the empty string gives those rows the oldest possible age, so they sort
// first and can be served from the index instead of forcing an OR that SQLite
// cannot use.
const lyricsStaleAge = "COALESCE(lyrics_checked_at, '')"

// ListTracksMissingLyrics returns tracks no lyrics provider has ever settled,
// oldest row first.
//
// The caller is expected to drop tracks that already have usable lyrics before
// spending an upstream request on them, so this deliberately does not filter on
// the lyrics database: the two are separate files and cannot be joined. Use
// TrackIDsWithUsableLyrics for the per-page check.
func ListTracksMissingLyrics(ctx context.Context, database *sql.DB, afterID int64, limit int) ([]LyricsWork, error) {
	if database == nil {
		return nil, errors.New("metadata database is nil")
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := database.QueryContext(ctx, `SELECT id, name, artist_name,
COALESCE(album_name, ''), COALESCE(duration, 0), COALESCE(isrc, '')
FROM tracks
WHERE lyrics_checked = 0 AND id > ?
ORDER BY id
LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tracks missing lyrics: %w", err)
	}
	defer rows.Close()

	work := make([]LyricsWork, 0, limit)
	for rows.Next() {
		var item LyricsWork
		if err := rows.Scan(&item.ID, &item.Name, &item.Artist, &item.Album, &item.Duration, &item.ISRC); err != nil {
			return nil, fmt.Errorf("scan track missing lyrics: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracks missing lyrics: %w", err)
	}
	return work, nil
}

// ListStaleLyrics returns tracks a lyrics provider settled before cutoff,
// oldest answer first. It is the second phase of a refresh run.
//
// It pages on (age, id) for the same reasons the metadata stale scan does: the
// order is by age, so the cursor has to be a tuple to be a valid resume point,
// and a row the run settles leaves the set while an unsettled row is still not
// re-read because the cursor only advances.
func ListStaleLyrics(ctx context.Context, database *sql.DB, cutoff time.Time, afterAge string, afterID int64, limit int) ([]LyricsWork, error) {
	if database == nil {
		return nil, errors.New("metadata database is nil")
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := database.QueryContext(ctx, `SELECT id, name, artist_name,
COALESCE(album_name, ''), COALESCE(duration, 0), COALESCE(isrc, ''), `+lyricsStaleAge+`
FROM tracks
WHERE lyrics_checked = 1
AND `+lyricsStaleAge+` < ?
AND (`+lyricsStaleAge+` > ? OR (`+lyricsStaleAge+` = ? AND id > ?))
ORDER BY `+lyricsStaleAge+`, id
LIMIT ?`, sqlTime(cutoff), afterAge, afterAge, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list stale lyrics: %w", err)
	}
	defer rows.Close()

	work := make([]LyricsWork, 0, limit)
	for rows.Next() {
		var item LyricsWork
		if err := rows.Scan(&item.ID, &item.Name, &item.Artist, &item.Album, &item.Duration, &item.ISRC, &item.CheckedAt); err != nil {
			return nil, fmt.Errorf("scan stale lyrics: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale lyrics: %w", err)
	}
	return work, nil
}

// CountTracksMissingLyrics returns how many tracks no lyrics provider has ever
// settled.
func CountTracksMissingLyrics(ctx context.Context, database *sql.DB) (int64, error) {
	if database == nil {
		return 0, errors.New("metadata database is nil")
	}
	var count int64
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM tracks WHERE lyrics_checked = 0").Scan(&count); err != nil {
		return 0, fmt.Errorf("count tracks missing lyrics: %w", err)
	}
	return count, nil
}

// CountStaleLyrics returns how many tracks a lyrics provider settled before
// cutoff.
func CountStaleLyrics(ctx context.Context, database *sql.DB, cutoff time.Time) (int64, error) {
	if database == nil {
		return 0, errors.New("metadata database is nil")
	}
	var count int64
	if err := database.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tracks WHERE lyrics_checked = 1 AND "+lyricsStaleAge+" < ?",
		sqlTime(cutoff)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count stale lyrics: %w", err)
	}
	return count, nil
}

// TrackIDsWithUsableLyrics reports which of the given tracks already have
// lyrics worth serving, so the backfill does not spend an upstream request
// re-confirming them.
//
// "Usable" means any of plain lyrics, synced lyrics, or an instrumental marker.
// An instrumental answer is a real answer: without the flag an instrumental track
// reads as having no lyrics and would be re-asked on every run.
//
// One query covers a whole page, because a per-track lookup would turn a
// 500-track page into 500 round trips to a database the live server is also
// writing.
func TrackIDsWithUsableLyrics(ctx context.Context, lyricsDB *sql.DB, trackIDs []int64) (map[int64]bool, error) {
	found := make(map[int64]bool, len(trackIDs))
	if lyricsDB == nil || len(trackIDs) == 0 {
		return found, nil
	}
	placeholders := make([]string, 0, len(trackIDs))
	args := make([]any, 0, len(trackIDs))
	for _, id := range trackIDs {
		if id <= 0 {
			continue
		}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(placeholders) == 0 {
		return found, nil
	}
	query := `SELECT DISTINCT lt.track_id
FROM lyrics_tracks AS lt
JOIN lyrics AS l ON l.id = lt.lyrics_id
WHERE lt.track_id IN (` + strings.Join(placeholders, ",") + `)
AND (l.has_plain_lyrics OR l.has_synced_lyrics OR l.instrumental)`
	rows, err := lyricsDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("find tracks with usable lyrics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan track with usable lyrics: %w", err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracks with usable lyrics: %w", err)
	}
	return found, nil
}

// MarkTrackLyricsChecked records that a lyrics provider gave a definitive answer
// for a track and no provider could do better. The row is flagged so later runs
// skip it by default, while leaving every cached field untouched.
//
// Only a definitive answer may call this. A throttle, a refusal, a timeout, or a
// network failure must leave the track unchecked, or a bad moment would
// permanently record that a song has no lyrics.
func MarkTrackLyricsChecked(ctx context.Context, tx *sql.Tx, trackID int64) error {
	if tx == nil {
		return errors.New("transaction is nil")
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE tracks SET lyrics_checked = 1, lyrics_checked_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ?", trackID); err != nil {
		return fmt.Errorf("mark lyrics checked: %w", err)
	}
	return nil
}

// MarkTrackLyricsSettledUnknownAge flags a track as already answered while
// deliberately leaving the settle time empty.
//
// This exists for the track that was cached before these columns existed. Such a
// track has a real answer but no record of when it was given, and inventing a
// time would be the worst of both: it would make every row in the existing
// library look freshly settled, so a refresh would skip the entire library on
// its first pass and leave the genuinely stale tracks for a later one.
//
// An empty age sorts as the oldest possible, so the track is skipped by a
// default run (it needs nothing) and still revisited by any refresh.
func MarkTrackLyricsSettledUnknownAge(ctx context.Context, tx *sql.Tx, trackID int64) error {
	if tx == nil {
		return errors.New("transaction is nil")
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE tracks SET lyrics_checked = 1, lyrics_checked_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?", trackID); err != nil {
		return fmt.Errorf("mark lyrics settled with unknown age: %w", err)
	}
	return nil
}
