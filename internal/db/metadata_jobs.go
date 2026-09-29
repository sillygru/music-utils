package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MetadataWork is one track awaiting an upstream metadata lookup, reduced to
// the fields needed to build a provider query plus the row identity used to
// write the result back.
type MetadataWork struct {
	ID        int64
	Name      string
	Artist    string
	Album     string
	Duration  float64
	CreatedAt string
}

// ListTracksMissingMetadata returns tracks that have never been resolved
// against an upstream metadata provider, oldest first so a backfill walks the
// library in the order it was imported. The result is streamed through rows so
// a library of any size can be processed without holding every track in memory.
func ListTracksMissingMetadata(ctx context.Context, database *sql.DB, afterID int64, limit int) ([]MetadataWork, error) {
	if database == nil {
		return nil, errors.New("metadata database is nil")
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := database.QueryContext(ctx, `SELECT id, name, artist_name,
COALESCE(album_name, ''), COALESCE(duration, 0), created_at
FROM tracks
WHERE metadata_checked = 0 AND id > ?
ORDER BY id
LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tracks missing metadata: %w", err)
	}
	defer rows.Close()

	work := make([]MetadataWork, 0, limit)
	for rows.Next() {
		var item MetadataWork
		if err := rows.Scan(&item.ID, &item.Name, &item.Artist, &item.Album, &item.Duration, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan track missing metadata: %w", err)
		}
		work = append(work, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracks missing metadata: %w", err)
	}
	return work, nil
}

// CountTracksMissingMetadata returns how many tracks have never been resolved
// against an upstream metadata provider.
func CountTracksMissingMetadata(ctx context.Context, database *sql.DB) (int64, error) {
	if database == nil {
		return 0, errors.New("metadata database is nil")
	}
	var count int64
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM tracks WHERE metadata_checked = 0").Scan(&count); err != nil {
		return 0, fmt.Errorf("count tracks missing metadata: %w", err)
	}
	return count, nil
}

// MarkTrackMetadataChecked records that a track was looked up upstream and no
// provider could resolve it. The row is flagged so later runs skip it, while
// leaving every already-cached field untouched.
//
// The timestamp is stored alongside the flag because the flag alone cannot be
// aged: a track marked a year ago and one marked a minute ago are both just
// `metadata_checked = 1`, so there is no way to tell a stale negative from a
// fresh one, or to find every miss and retry it once a provider is likely to
// know better.
//
// This is a definitive answer, so it always restamps. Only a write that carries
// no answer at all leaves the timestamp alone.
func MarkTrackMetadataChecked(ctx context.Context, tx *sql.Tx, trackID int64) error {
	if tx == nil {
		return errors.New("transaction is nil")
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE tracks SET metadata_checked = 1, metadata_checked_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = ?", trackID); err != nil {
		return fmt.Errorf("mark metadata checked: %w", err)
	}
	return nil
}

// MergeTrackMetadata folds a provider result into the existing track row
// addressed by ID.
//
// Unlike UpsertTrackMetadata it never inserts a row and never re-keys on album
// or duration, so a provider returning a different release variant enriches the
// original row instead of creating a near-duplicate. Every field is merged
// conservatively: a blank or zero incoming value leaves the stored value alone,
// and the track's identity (name, artist, album, duration) is only corrected
// when the existing row is missing it. This makes the write safe to re-run and
// safe to interleave with the live server, which reads the same rows. If filling
// missing identity fields would collide with another row's unique track key, the
// merge leaves those identity fields alone while still storing other metadata.
//
// metadata_source is the one field always taken from the provider when present:
// a row cached from lyrics carries the lyrics provider there, and once real
// upstream metadata is resolved that stale value would misreport where the
// genre and year actually came from.
func MergeTrackMetadata(ctx context.Context, tx *sql.Tx, trackID int64, track Track) error {
	if tx == nil {
		return errors.New("transaction is nil")
	}
	normalizeTrack(&track)
	_, err := tx.ExecContext(ctx, `WITH target_identity AS (
SELECT id,
CASE WHEN name_lower='' THEN ? ELSE name_lower END AS name_lower,
CASE WHEN artist_name_lower='' THEN ? ELSE artist_name_lower END AS artist_name_lower,
CASE WHEN COALESCE(album_name_lower,'')='' THEN NULLIF(?,'') ELSE album_name_lower END AS album_name_lower,
CASE WHEN COALESCE(duration,0)<=0 AND ?>0 THEN ? ELSE duration END AS duration
FROM tracks WHERE id=?
), identity_safety AS (
SELECT target_identity.id,
NOT EXISTS (
	SELECT 1 FROM tracks AS other
	WHERE other.id<>target_identity.id
	AND other.name_lower=target_identity.name_lower
	AND other.artist_name_lower=target_identity.artist_name_lower
	AND other.album_name_lower=target_identity.album_name_lower
	AND other.duration=target_identity.duration
) AS can_update
FROM target_identity
)
UPDATE tracks SET
name=CASE WHEN name='' AND (SELECT can_update FROM identity_safety WHERE id=tracks.id)=1 THEN ? ELSE name END,
name_lower=CASE WHEN name_lower='' AND (SELECT can_update FROM identity_safety WHERE id=tracks.id)=1 THEN ? ELSE name_lower END,
artist_name=CASE WHEN artist_name='' AND (SELECT can_update FROM identity_safety WHERE id=tracks.id)=1 THEN ? ELSE artist_name END,
artist_name_lower=CASE WHEN artist_name_lower='' AND (SELECT can_update FROM identity_safety WHERE id=tracks.id)=1 THEN ? ELSE artist_name_lower END,
album_name=CASE WHEN COALESCE(album_name,'')='' AND (SELECT can_update FROM identity_safety WHERE id=tracks.id)=1 THEN NULLIF(?,'') ELSE album_name END,
album_name_lower=CASE WHEN COALESCE(album_name_lower,'')='' AND (SELECT can_update FROM identity_safety WHERE id=tracks.id)=1 THEN NULLIF(?,'') ELSE album_name_lower END,
duration=CASE WHEN COALESCE(duration,0)<=0 AND ?>0 AND (SELECT can_update FROM identity_safety WHERE id=tracks.id)=1 THEN ? ELSE duration END,
genre=CASE WHEN ?<>'' THEN ? ELSE genre END,
genre_lower=CASE WHEN ?<>'' THEN ? ELSE genre_lower END,
year=CASE WHEN ?>0 THEN ? ELSE year END,
release_date=CASE WHEN ?<>'' THEN ? ELSE release_date END,
isrc=CASE WHEN COALESCE(isrc,'')='' THEN NULLIF(?,'') ELSE isrc END,
musicbrainz_recording_id=CASE WHEN COALESCE(musicbrainz_recording_id,'')='' THEN NULLIF(?,'') ELSE musicbrainz_recording_id END,
musicbrainz_release_id=CASE WHEN COALESCE(musicbrainz_release_id,'')='' THEN NULLIF(?,'') ELSE musicbrainz_release_id END,
musicbrainz_release_group_id=CASE WHEN COALESCE(musicbrainz_release_group_id,'')='' THEN NULLIF(?,'') ELSE musicbrainz_release_group_id END,
musicbrainz_artist_id=CASE WHEN COALESCE(musicbrainz_artist_id,'')='' THEN NULLIF(?,'') ELSE musicbrainz_artist_id END,
cover_url=CASE WHEN COALESCE(cover_url,'')='' THEN NULLIF(?,'') ELSE cover_url END,
cover_url_source=CASE WHEN COALESCE(cover_url_source,'')='' THEN NULLIF(?,'') ELSE cover_url_source END,
metadata_source=CASE WHEN ?<>'' THEN ? ELSE metadata_source END,
cover_url_checked=MAX(cover_url_checked, ?),
metadata_checked=1,
metadata_checked_at=CURRENT_TIMESTAMP,
updated_at=CURRENT_TIMESTAMP
WHERE id = ?`,
		track.NameLower, track.ArtistNameLower, track.AlbumNameLower,
		track.Duration, track.Duration, trackID,
		track.Name, track.NameLower,
		track.ArtistName, track.ArtistNameLower,
		track.AlbumName, track.AlbumNameLower,
		track.Duration, track.Duration,
		track.Genre, track.Genre, track.GenreLower, track.GenreLower,
		track.Year, track.Year,
		track.ReleaseDate, track.ReleaseDate,
		track.ISRC,
		track.MusicBrainzRecordingID, track.MusicBrainzReleaseID, track.MusicBrainzReleaseGroupID, track.MusicBrainzArtistID,
		track.CoverURL, track.CoverURLSource,
		track.MetadataSource, track.MetadataSource,
		track.CoverURLChecked,
		trackID)
	if err != nil {
		return fmt.Errorf("merge track metadata: %w", err)
	}
	return nil
}
