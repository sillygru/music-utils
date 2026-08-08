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

// FindTrackExact loads metadata from metadataDB and lyrics from lyricsDB.
func FindTrackExact(ctx context.Context, metadataDB, lyricsDB *sql.DB, name, artist, album string, duration float64) (*Track, *Lyrics, error) {
	track, err := FindTrackMetadataExact(ctx, metadataDB, name, artist, album, duration)
	if err != nil {
		return nil, nil, err
	}
	lyrics := &Lyrics{}
	if track.LastLyricsID > 0 && lyricsDB != nil {
		if found, lookupErr := FindLyricsByID(ctx, lyricsDB, track.LastLyricsID); lookupErr == nil {
			lyrics = found
		} else if !errors.Is(lookupErr, sql.ErrNoRows) {
			return nil, nil, lookupErr
		}
	}
	return track, lyrics, nil
}

// FindTrackMetadataExact loads a track only from the metadata database.
func FindTrackMetadataExact(ctx context.Context, database *sql.DB, name, artist, album string, duration float64) (*Track, error) {
	if database == nil {
		return nil, errors.New("metadata database is nil")
	}
	query := "SELECT " + trackColumns("tracks") + " FROM tracks WHERE name_lower = ? AND artist_name_lower = ?"
	args := []any{normalize(name), normalize(artist)}
	if value := normalize(album); value != "" {
		query += " AND album_name_lower = ?"
		args = append(args, value)
	}
	if duration > 0 {
		query += " AND duration = ?"
		args = append(args, duration)
	}
	query += " LIMIT 1"
	track := &Track{}
	err := database.QueryRowContext(ctx, query, args...).Scan(trackScanArgs(track)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("find track metadata: %w", err)
	}
	return track, nil
}

// UpsertTrackMetadata stores provider metadata only in metadataDB.
func UpsertTrackMetadata(ctx context.Context, database *sql.DB, track Track) (int64, error) {
	if database == nil {
		return 0, errors.New("metadata database is nil")
	}
	if track.Source == "" {
		track.Source = "external"
	}
	normalizeTrack(&track)
	if track.MetadataSource == "" {
		track.MetadataSource = track.Source
	}
	if track.MusicBrainzRecordingID != "" {
		var existingID int64
		err := database.QueryRowContext(ctx, "SELECT id FROM tracks WHERE musicbrainz_recording_id = ? LIMIT 1", track.MusicBrainzRecordingID).Scan(&existingID)
		if err == nil {
			_, err = database.ExecContext(ctx, `UPDATE tracks SET
name=?, name_lower=?, artist_name=?, artist_name_lower=?, album_name=?, album_name_lower=?,
duration=CASE WHEN ? > 0 THEN ? ELSE duration END,
genre=CASE WHEN ? <> '' THEN ? ELSE genre END, genre_lower=CASE WHEN ? <> '' THEN ? ELSE genre_lower END,
year=CASE WHEN ? > 0 THEN ? ELSE year END, release_date=CASE WHEN ? <> '' THEN ? ELSE release_date END,
isrc=CASE WHEN ? <> '' THEN ? ELSE isrc END, musicbrainz_release_id=CASE WHEN ? <> '' THEN ? ELSE musicbrainz_release_id END,
musicbrainz_release_group_id=CASE WHEN ? <> '' THEN ? ELSE musicbrainz_release_group_id END,
musicbrainz_artist_id=CASE WHEN ? <> '' THEN ? ELSE musicbrainz_artist_id END,
cover_url=CASE WHEN ? <> '' THEN ? ELSE cover_url END, metadata_source=CASE WHEN ? <> '' THEN ? ELSE metadata_source END,
cover_url_source=CASE WHEN ? <> '' THEN ? ELSE cover_url_source END,
metadata_checked=MAX(metadata_checked, ?), cover_url_checked=MAX(cover_url_checked, ?),
updated_at=CURRENT_TIMESTAMP, source=? WHERE id=?`,
				track.Name, track.NameLower, track.ArtistName, track.ArtistNameLower, track.AlbumName, track.AlbumNameLower,
				track.Duration, track.Duration, track.Genre, track.Genre, track.GenreLower, track.GenreLower, track.Year, track.Year,
				track.ReleaseDate, track.ReleaseDate, track.ISRC, track.ISRC, track.MusicBrainzReleaseID, track.MusicBrainzReleaseID,
				track.MusicBrainzReleaseGroupID, track.MusicBrainzReleaseGroupID, track.MusicBrainzArtistID, track.MusicBrainzArtistID,
				track.CoverURL, track.CoverURL, track.MetadataSource, track.MetadataSource, track.CoverURLSource, track.CoverURLSource,
				track.MetadataChecked, track.CoverURLChecked, track.Source, existingID)
			if err != nil {
				return 0, fmt.Errorf("update metadata by recording ID: %w", err)
			}
			return existingID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("find metadata by recording ID: %w", err)
		}
	}
	const statement = `INSERT INTO tracks (
name,name_lower,artist_name,artist_name_lower,album_name,album_name_lower,duration,
genre,genre_lower,year,release_date,isrc,musicbrainz_recording_id,musicbrainz_release_id,
musicbrainz_release_group_id,musicbrainz_artist_id,cover_url,metadata_source,cover_url_source,
metadata_checked,cover_url_checked,source) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(name_lower,artist_name_lower,album_name_lower,duration) DO UPDATE SET
name=excluded.name, artist_name=excluded.artist_name, album_name=excluded.album_name,
genre=CASE WHEN excluded.genre<>'' THEN excluded.genre ELSE tracks.genre END,
genre_lower=CASE WHEN excluded.genre_lower<>'' THEN excluded.genre_lower ELSE tracks.genre_lower END,
year=CASE WHEN excluded.year>0 THEN excluded.year ELSE tracks.year END,
release_date=CASE WHEN excluded.release_date<>'' THEN excluded.release_date ELSE tracks.release_date END,
isrc=CASE WHEN excluded.isrc<>'' THEN excluded.isrc ELSE tracks.isrc END,
musicbrainz_recording_id=CASE WHEN excluded.musicbrainz_recording_id<>'' THEN excluded.musicbrainz_recording_id ELSE tracks.musicbrainz_recording_id END,
musicbrainz_release_id=CASE WHEN excluded.musicbrainz_release_id<>'' THEN excluded.musicbrainz_release_id ELSE tracks.musicbrainz_release_id END,
musicbrainz_release_group_id=CASE WHEN excluded.musicbrainz_release_group_id<>'' THEN excluded.musicbrainz_release_group_id ELSE tracks.musicbrainz_release_group_id END,
musicbrainz_artist_id=CASE WHEN excluded.musicbrainz_artist_id<>'' THEN excluded.musicbrainz_artist_id ELSE tracks.musicbrainz_artist_id END,
cover_url=CASE WHEN excluded.cover_url<>'' THEN excluded.cover_url ELSE tracks.cover_url END,
metadata_source=CASE WHEN excluded.metadata_source<>'' THEN excluded.metadata_source ELSE tracks.metadata_source END,
cover_url_source=CASE WHEN excluded.cover_url_source<>'' THEN excluded.cover_url_source ELSE tracks.cover_url_source END,
metadata_checked=MAX(tracks.metadata_checked,excluded.metadata_checked), cover_url_checked=MAX(tracks.cover_url_checked,excluded.cover_url_checked),
updated_at=CURRENT_TIMESTAMP, source=excluded.source RETURNING id`
	args := []any{track.Name, track.NameLower, track.ArtistName, track.ArtistNameLower, track.AlbumName, track.AlbumNameLower, track.Duration,
		nullableText(track.Genre), nullableText(track.GenreLower), track.Year, nullableText(track.ReleaseDate), nullableText(track.ISRC),
		nullableText(track.MusicBrainzRecordingID), nullableText(track.MusicBrainzReleaseID), nullableText(track.MusicBrainzReleaseGroupID), nullableText(track.MusicBrainzArtistID),
		nullableText(track.CoverURL), nullableText(track.MetadataSource), nullableText(track.CoverURLSource), track.MetadataChecked, track.CoverURLChecked, track.Source}
	if err := database.QueryRowContext(ctx, statement, args...).Scan(&track.ID); err != nil {
		return 0, fmt.Errorf("upsert metadata: %w", err)
	}
	return track.ID, nil
}

// InsertTrackWithLyrics writes metadata and lyrics to their respective databases.
// Metadata remains valid if the separate lyrics write fails.
func InsertTrackWithLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB, track Track, lyrics Lyrics) (trackID, lyricsID int64, err error) {
	if metadataDB == nil || lyricsDB == nil {
		return 0, 0, errors.New("metadata and lyrics databases are required")
	}
	if track.Source == "" {
		track.Source = "local"
	}
	normalizeTrack(&track)
	if lyrics.Source == "" {
		lyrics.Source = track.Source
	}
	if lyrics.ContentHash == "" {
		lyrics.ContentHash = contentHash(lyrics.PlainLyrics, lyrics.SyncedLyrics)
	}
	lyrics.HasPlain = lyrics.HasPlain || lyrics.PlainLyrics != ""
	lyrics.HasSynced = lyrics.HasSynced || lyrics.SyncedLyrics != ""
	trackID, err = UpsertTrackMetadata(ctx, metadataDB, track)
	if err != nil {
		return 0, 0, err
	}
	lyricsTx, err := lyricsDB.BeginTx(ctx, nil)
	if err != nil {
		return trackID, 0, fmt.Errorf("begin lyrics insert: %w", err)
	}
	defer func() {
		if err != nil {
			_ = lyricsTx.Rollback()
		}
	}()
	if err = lyricsTx.QueryRowContext(ctx, `INSERT OR IGNORE INTO lyrics (track_id,plain_lyrics,synced_lyrics,has_plain_lyrics,has_synced_lyrics,instrumental,content_hash,source) VALUES (?,?,?,?,?,?,?,?) RETURNING id`, trackID, nullableText(lyrics.PlainLyrics), nullableText(lyrics.SyncedLyrics), lyrics.HasPlain, lyrics.HasSynced, lyrics.Instrumental, lyrics.ContentHash, lyrics.Source).Scan(&lyricsID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return trackID, 0, fmt.Errorf("insert lyrics: %w", err)
		}
		if err = lyricsTx.QueryRowContext(ctx, `SELECT id FROM lyrics WHERE content_hash=? LIMIT 1`, lyrics.ContentHash).Scan(&lyricsID); err != nil {
			return trackID, 0, fmt.Errorf("select lyrics: %w", err)
		}
	}
	if _, err = lyricsTx.ExecContext(ctx, `INSERT OR IGNORE INTO lyrics_tracks(track_id,lyrics_id) VALUES(?,?)`, trackID, lyricsID); err != nil {
		return trackID, 0, fmt.Errorf("associate lyrics: %w", err)
	}
	if err = lyricsTx.Commit(); err != nil {
		return trackID, 0, fmt.Errorf("commit lyrics: %w", err)
	}
	if _, err = metadataDB.ExecContext(ctx, `UPDATE tracks SET last_lyrics_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, lyricsID, trackID); err != nil {
		return trackID, lyricsID, fmt.Errorf("link lyrics reference: %w", err)
	}
	return trackID, lyricsID, nil
}

// FindLyricsByID reads one row from the lyrics database.
func FindLyricsByID(ctx context.Context, database *sql.DB, lyricsID int64) (*Lyrics, error) {
	if database == nil {
		return nil, errors.New("lyrics database is nil")
	}
	lyrics := &Lyrics{}
	err := database.QueryRowContext(ctx, `SELECT id,track_id,COALESCE(plain_lyrics,''),COALESCE(synced_lyrics,''),has_plain_lyrics,has_synced_lyrics,instrumental,content_hash,source FROM lyrics WHERE id=? LIMIT 1`, lyricsID).Scan(&lyrics.ID, &lyrics.TrackID, &lyrics.PlainLyrics, &lyrics.SyncedLyrics, &lyrics.HasPlain, &lyrics.HasSynced, &lyrics.Instrumental, &lyrics.ContentHash, &lyrics.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("find lyrics by id: %w", err)
	}
	return lyrics, nil
}

// FindCoverArt loads one cached album or artist cover row from the cover
// database. It returns sql.ErrNoRows when nothing has been checked yet.
func FindCoverArt(ctx context.Context, database *sql.DB, entityType CoverEntity, artistName, albumName string) (*CoverArt, error) {
	if database == nil {
		return nil, errors.New("cover database is nil")
	}
	if entityType != CoverArtist && entityType != CoverAlbum {
		return nil, fmt.Errorf("invalid cover entity type %q", entityType)
	}
	album := normalize(albumName)
	if entityType == CoverArtist {
		album = ""
	}
	cover := &CoverArt{}
	err := database.QueryRowContext(ctx, "SELECT id,entity_type,COALESCE(artist_name_lower,''),COALESCE(album_name_lower,''),COALESCE(cover_url,''),COALESCE(cover_source,''),COALESCE(checked_at,'') FROM cover_urls WHERE entity_type=? AND artist_name_lower=? AND COALESCE(album_name_lower,'')=? LIMIT 1",
		string(entityType), normalize(artistName), album).
		Scan(&cover.ID, &cover.EntityType, &cover.ArtistNameLower, &cover.AlbumNameLower, &cover.CoverURL, &cover.CoverSource, &cover.CheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("find cover art: %w", err)
	}
	return cover, nil
}

// UpsertCoverArt caches an album or artist cover URL. A hit stores the resolved
// URL and its source; a miss stores a NULL URL with a set checked_at so the
// negative result is memoized.
func UpsertCoverArt(ctx context.Context, database *sql.DB, entityType CoverEntity, artistName, albumName, coverURL, coverSource string) error {
	if database == nil {
		return errors.New("cover database is nil")
	}
	if entityType != CoverArtist && entityType != CoverAlbum {
		return fmt.Errorf("invalid cover entity type %q", entityType)
	}
	// Artist rows store '' (not NULL) for the album column: SQLite treats
	// NULLs as distinct in UNIQUE indexes, so NULL would make the ON CONFLICT
	// never match and every re-upsert would insert a duplicate row.
	album := albumType(albumName)
	var urlValue any
	if coverURL != "" {
		urlValue = coverURL
	}
	var sourceValue any
	if coverSource != "" {
		sourceValue = coverSource
	}
	_, err := database.ExecContext(ctx, `INSERT INTO cover_urls (entity_type,artist_name_lower,album_name_lower,cover_url,cover_source,checked_at,updated_at)
VALUES (?,?,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)
ON CONFLICT(entity_type,artist_name_lower,album_name_lower) DO UPDATE SET
cover_url=CASE WHEN excluded.cover_url IS NOT NULL THEN excluded.cover_url ELSE cover_url END,
cover_source=CASE WHEN excluded.cover_url IS NOT NULL THEN excluded.cover_source ELSE cover_source END,
checked_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP`,
		string(entityType), normalize(artistName), album, urlValue, sourceValue)
	if err != nil {
		return fmt.Errorf("upsert cover art: %w", err)
	}
	return nil
}

// ExpireCoverArt step is intentionally omitted: negative-cache expiry is handled
// by callers comparing CheckedAt against a TTL, not by row deletion.

type TrackSearchResult struct {
	Track
	Lyrics
}

// SearchTracks searches metadata FTS and composes matching lyrics rows in Go.
func SearchTracks(ctx context.Context, metadataDB, lyricsDB *sql.DB, query string, limit int) ([]TrackSearchResult, error) {
	if metadataDB == nil {
		return nil, errors.New("metadata database is nil")
	}
	if limit < 1 {
		return []TrackSearchResult{}, nil
	}
	if limit > 100 {
		limit = 100
	}
	match := ftsQuery(query)
	if match == "" {
		return []TrackSearchResult{}, nil
	}
	rows, err := metadataDB.QueryContext(ctx, `SELECT `+trackColumns("t")+` FROM tracks_fts AS f JOIN tracks AS t ON t.id=f.rowid WHERE tracks_fts MATCH ? ORDER BY t.id LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("search metadata: %w", err)
	}
	defer rows.Close()
	result := make([]TrackSearchResult, 0, limit)
	for rows.Next() {
		track := &Track{}
		if err = rows.Scan(trackScanArgs(track)...); err != nil {
			return nil, fmt.Errorf("scan metadata search: %w", err)
		}
		lyrics := Lyrics{}
		if track.LastLyricsID > 0 && lyricsDB != nil {
			if found, lookupErr := FindLyricsByID(ctx, lyricsDB, track.LastLyricsID); lookupErr == nil {
				lyrics = *found
			} else if !errors.Is(lookupErr, sql.ErrNoRows) {
				return nil, lookupErr
			}
		}
		result = append(result, TrackSearchResult{Track: *track, Lyrics: lyrics})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate metadata search: %w", err)
	}
	return result, nil
}

func trackColumns(alias string) string {
	return alias + `.id, ` + alias + `.name, ` + alias + `.name_lower, ` + alias + `.artist_name, ` + alias + `.artist_name_lower, COALESCE(` + alias + `.album_name,''), COALESCE(` + alias + `.album_name_lower,''), COALESCE(` + alias + `.duration,0), COALESCE(` + alias + `.genre,''), COALESCE(` + alias + `.genre_lower,''), COALESCE(` + alias + `.year,0), COALESCE(` + alias + `.release_date,''), COALESCE(` + alias + `.isrc,''), COALESCE(` + alias + `.musicbrainz_recording_id,''), COALESCE(` + alias + `.musicbrainz_release_id,''), COALESCE(` + alias + `.musicbrainz_release_group_id,''), COALESCE(` + alias + `.musicbrainz_artist_id,''), COALESCE(` + alias + `.cover_url,''), COALESCE(` + alias + `.metadata_source,''), COALESCE(` + alias + `.cover_url_source,''), COALESCE(` + alias + `.metadata_checked,0), COALESCE(` + alias + `.cover_url_checked,0), COALESCE(` + alias + `.last_lyrics_id,0), COALESCE(` + alias + `.source,'' )`
}
func trackScanArgs(track *Track) []any {
	return []any{&track.ID, &track.Name, &track.NameLower, &track.ArtistName, &track.ArtistNameLower, &track.AlbumName, &track.AlbumNameLower, &track.Duration, &track.Genre, &track.GenreLower, &track.Year, &track.ReleaseDate, &track.ISRC, &track.MusicBrainzRecordingID, &track.MusicBrainzReleaseID, &track.MusicBrainzReleaseGroupID, &track.MusicBrainzArtistID, &track.CoverURL, &track.MetadataSource, &track.CoverURLSource, &track.MetadataChecked, &track.CoverURLChecked, &track.LastLyricsID, &track.Source}
}
func normalizeTrack(track *Track) {
	track.Name = strings.TrimSpace(track.Name)
	track.ArtistName = strings.TrimSpace(track.ArtistName)
	track.AlbumName = strings.TrimSpace(track.AlbumName)
	track.NameLower = normalize(track.Name)
	track.ArtistNameLower = normalize(track.ArtistName)
	track.AlbumNameLower = normalize(track.AlbumName)
	track.Genre = strings.TrimSpace(track.Genre)
	track.GenreLower = normalize(track.Genre)
	if track.MetadataSource == "" {
		track.MetadataSource = track.Source
	}
}
func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

// albumType normalizes an album name, returning "" for artist-entity rows so the
// album column stays NULL there.
func albumType(value string) string { return normalize(value) }
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
