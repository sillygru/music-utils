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
	query := "SELECT " + trackColumns("tracks") + " FROM tracks WHERE name_lower = ?"
	args := []any{normalize(name)}
	if artist = normalize(artist); artist != "" {
		query += " AND artist_name_lower = ?"
		args = append(args, artist)
	}
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

// UpsertRichLyrics stores one source-native rich/syllable payload for a track.
// Rich content is kept separate from synced_lyrics so existing LRCLIB clients
// continue receiving the same LRC-compatible field.
func UpsertRichLyrics(ctx context.Context, database *sql.DB, rich RichLyrics) error {
	if database == nil {
		return errors.New("lyrics database is nil")
	}
	rich.Content = strings.TrimSpace(rich.Content)
	rich.Format = strings.ToLower(strings.TrimSpace(rich.Format))
	rich.SyncType = strings.ToLower(strings.TrimSpace(rich.SyncType))
	rich.Source = strings.ToLower(strings.TrimSpace(rich.Source))
	if rich.TrackID <= 0 || rich.Content == "" || rich.Format == "" || rich.SyncType == "" || rich.Source == "" {
		return errors.New("rich lyrics track, content, format, sync type, and source are required")
	}
	if rich.Hash == "" {
		rich.Hash = richContentHash(rich.Content, rich.Format, rich.SyncType)
	}
	_, err := database.ExecContext(ctx, `INSERT INTO lyrics_sync_variants (track_id,content,format,sync_type,source,content_hash,updated_at)
VALUES (?,?,?,?,?,?,CURRENT_TIMESTAMP)
ON CONFLICT(track_id,format,sync_type,source) DO UPDATE SET
content=excluded.content, content_hash=excluded.content_hash, updated_at=CURRENT_TIMESTAMP`,
		rich.TrackID, rich.Content, rich.Format, rich.SyncType, rich.Source, rich.Hash)
	if err != nil {
		return fmt.Errorf("upsert rich lyrics: %w", err)
	}
	return nil
}

// RichLyricsConverter converts one source-native rich payload for storage. The
// bool reports whether the converter produced a replacement.
type RichLyricsConverter func(content, format string) (newContent, newFormat string, ok bool)

// MigrateRichLyrics converts existing rich payload rows without holding a
// transaction across the whole table. Callers can run this in the background
// so startup is not delayed while the database is upgraded.
func MigrateRichLyrics(ctx context.Context, database *sql.DB, converter RichLyricsConverter) (int, error) {
	if database == nil {
		return 0, errors.New("lyrics database is nil")
	}
	if converter == nil {
		return 0, errors.New("rich lyrics converter is nil")
	}
	rows, err := database.QueryContext(ctx, `SELECT id,content,format,sync_type FROM lyrics_sync_variants WHERE LOWER(format)='ttml'`)
	if err != nil {
		return 0, fmt.Errorf("find rich lyrics to migrate: %w", err)
	}
	type migrationRow struct {
		id       int64
		content  string
		format   string
		syncType string
	}
	pending := make([]migrationRow, 0)
	for rows.Next() {
		var row migrationRow
		if err := rows.Scan(&row.id, &row.content, &row.format, &row.syncType); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan rich lyrics migration: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate rich lyrics migration: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close rich lyrics migration rows: %w", err)
	}

	migrated := 0
	for _, row := range pending {
		content, format, ok := converter(row.content, row.format)
		if !ok {
			continue
		}
		result, err := database.ExecContext(ctx, `UPDATE lyrics_sync_variants
SET content=?, format=?, content_hash=?, updated_at=CURRENT_TIMESTAMP
WHERE id=? AND LOWER(format)='ttml'`, content, format, richContentHash(content, format, row.syncType), row.id)
		if err != nil {
			return migrated, fmt.Errorf("migrate rich lyrics %d: %w", row.id, err)
		}
		if affected, err := result.RowsAffected(); err == nil && affected > 0 {
			migrated++
		}
	}
	return migrated, nil
}

// FindRichLyrics returns the best cached rich payload for a track. A requested
// sync type narrows the result; an empty sync type accepts word or syllable
// variants in source priority order.
func FindRichLyrics(ctx context.Context, database *sql.DB, trackID int64, syncType string) (*RichLyrics, error) {
	if database == nil {
		return nil, errors.New("lyrics database is nil")
	}
	rich := &RichLyrics{}
	err := database.QueryRowContext(ctx, `SELECT id,track_id,content,format,sync_type,source,content_hash
FROM lyrics_sync_variants
WHERE track_id=? AND (?='' OR sync_type=?)
ORDER BY CASE sync_type WHEN 'word' THEN 0 WHEN 'syllable' THEN 1 WHEN 'richsync' THEN 2 ELSE 3 END,
         CASE source WHEN 'unison' THEN 0 ELSE 1 END, id DESC LIMIT 1`,
		trackID, strings.ToLower(strings.TrimSpace(syncType)), strings.ToLower(strings.TrimSpace(syncType))).Scan(
		&rich.ID, &rich.TrackID, &rich.Content, &rich.Format, &rich.SyncType, &rich.Source, &rich.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("find rich lyrics: %w", err)
	}
	return rich, nil
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

// UpsertCoverArt caches a single album or artist cover URL (the winner). A hit
// stores the resolved URL and its source; a miss stores a NULL URL with a set
// checked_at so the negative result is memoized. The URL is also recorded as
// the rank-0 variant so the variants table stays consistent for existing call
// sites.
func UpsertCoverArt(ctx context.Context, database *sql.DB, entityType CoverEntity, artistName, albumName, coverURL, coverSource string) error {
	var variants []CoverVariant
	if coverURL != "" {
		variants = []CoverVariant{{URL: coverURL, Source: coverSource}}
	}
	return UpsertCoverArtVariants(ctx, database, entityType, artistName, albumName, variants)
}

// UpsertCoverArtVariants stores the full set of cover URLs for an album or
// artist. The first variant becomes the winner mirrored on the parent
// cover_urls row, and every variant is kept in the variants table in rank
// order. A miss (no variants) stores a negative row so the lookup is memoized;
// the previous variant set is replaced in the same transaction.
func UpsertCoverArtVariants(ctx context.Context, database *sql.DB, entityType CoverEntity, artistName, albumName string, variants []CoverVariant) error {
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
	var winnerURL, winnerSource string
	if len(variants) > 0 {
		winnerURL, winnerSource = variants[0].URL, variants[0].Source
	}
	var urlValue, sourceValue any
	if winnerURL != "" {
		urlValue = winnerURL
	}
	if winnerSource != "" {
		sourceValue = winnerSource
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cover upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var coverURLID int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO cover_urls (entity_type,artist_name_lower,album_name_lower,cover_url,cover_source,checked_at,updated_at)
VALUES (?,?,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)
ON CONFLICT(entity_type,artist_name_lower,album_name_lower) DO UPDATE SET
cover_url=CASE WHEN excluded.cover_url IS NOT NULL THEN excluded.cover_url ELSE cover_url END,
cover_source=CASE WHEN excluded.cover_url IS NOT NULL THEN excluded.cover_source ELSE cover_source END,
checked_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP RETURNING id`,
		string(entityType), normalize(artistName), album, urlValue, sourceValue).Scan(&coverURLID); err != nil {
		return fmt.Errorf("upsert cover art: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM cover_url_variants WHERE cover_url_id = ?`, coverURLID); err != nil {
		return fmt.Errorf("clear cover variants: %w", err)
	}
	for i, variant := range variants {
		if strings.TrimSpace(variant.URL) == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO cover_url_variants (cover_url_id,url,source,rank) VALUES (?,?,?,?)`,
			coverURLID, variant.URL, nullableText(variant.Source), i); err != nil {
			return fmt.Errorf("insert cover variant: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit cover upsert: %w", err)
	}
	return nil
}

// FindCoverVariants returns every cached cover URL for an album or artist in
// rank order (0 = the winner on the parent row). Empty when the row predates
// the variants table or was stored as a negative miss.
func FindCoverVariants(ctx context.Context, database *sql.DB, coverURLID int64) ([]CoverVariant, error) {
	if database == nil {
		return nil, errors.New("cover database is nil")
	}
	rows, err := database.QueryContext(ctx, `SELECT COALESCE(url,''), COALESCE(source,''), rank FROM cover_url_variants WHERE cover_url_id = ? ORDER BY rank ASC`, coverURLID)
	if err != nil {
		return nil, fmt.Errorf("find cover variants: %w", err)
	}
	defer rows.Close()
	variants := []CoverVariant{}
	for rows.Next() {
		var variant CoverVariant
		if err = rows.Scan(&variant.URL, &variant.Source, &variant.Rank); err != nil {
			return nil, fmt.Errorf("scan cover variant: %w", err)
		}
		variants = append(variants, variant)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cover variants: %w", err)
	}
	return variants, nil
}

// PromoteCoverVariant swaps a live alternate into the winner slot of a cover
// row: the parent row's cover_url/cover_source become the promoted URL, the
// two ranks are exchanged so ordering stays consistent, and checked_at is
// bumped (the promoted URL was just validated).
func PromoteCoverVariant(ctx context.Context, database *sql.DB, coverURLID int64, url, source string, promotedRank int) error {
	if database == nil {
		return errors.New("cover database is nil")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cover promotion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE cover_urls SET cover_url=?, cover_source=?, checked_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		url, nullableText(source), coverURLID); err != nil {
		return fmt.Errorf("promote cover variant: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE cover_url_variants SET rank = CASE WHEN rank = 0 THEN ? WHEN rank = ? THEN 0 ELSE rank END WHERE cover_url_id = ?`,
		promotedRank, promotedRank, coverURLID); err != nil {
		return fmt.Errorf("reorder cover variants: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit cover promotion: %w", err)
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

// CacheStats holds the complete breakdown of cached content counts across
// metadata, lyrics, and cover databases.
type CacheStats struct {
	MetadataSongs int64
	LyricsSongs   int64
	SongCovers    int64
	AlbumCovers   int64
	ArtistCovers  int64
	TotalCovers   int64
	UniqueSongs   int64
	TotalCached   int64
}

// GetCacheStats computes the aggregated cache statistics across metadata, lyrics,
// and cover databases.
func GetCacheStats(ctx context.Context, metadataDB, lyricsDB, coverDB *sql.DB) (CacheStats, error) {
	var stats CacheStats
	if metadataDB != nil {
		if count, err := CountTracks(ctx, metadataDB); err == nil {
			stats.MetadataSongs = count
		} else if !isNoSuchTable(err) {
			return stats, err
		}
		if count, err := CountDistinctTrackNames(ctx, metadataDB); err == nil {
			stats.UniqueSongs = count
		} else if !isNoSuchTable(err) {
			return stats, err
		}
		if counts, err := CountCovers(ctx, metadataDB, coverDB); err == nil {
			stats.SongCovers = counts.Songs
			stats.AlbumCovers = counts.Albums
			stats.ArtistCovers = counts.Artists
			stats.TotalCovers = counts.Total()
		} else if !isNoSuchTable(err) {
			return stats, err
		}
	} else if coverDB != nil {
		if counts, err := CountCovers(ctx, nil, coverDB); err == nil {
			stats.AlbumCovers = counts.Albums
			stats.ArtistCovers = counts.Artists
			stats.TotalCovers = counts.Total()
		} else if !isNoSuchTable(err) {
			return stats, err
		}
	}
	if lyricsDB != nil {
		if count, err := CountLyricsTracks(ctx, lyricsDB); err == nil {
			stats.LyricsSongs = count
		} else if !isNoSuchTable(err) {
			return stats, err
		}
	}
	stats.TotalCached = stats.MetadataSongs + stats.LyricsSongs + stats.TotalCovers
	return stats, nil
}

func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such table")
}

// CoverCounts breaks down cached cover entries by what they cover. Songs come
// from the metadata cache (a track with a cover URL), albums and artists from
// the dedicated cover URL cache.
type CoverCounts struct {
	Songs   int64
	Albums  int64
	Artists int64
}

// Total returns the combined number of cached cover entries.
func (c CoverCounts) Total() int64 { return c.Songs + c.Albums + c.Artists }

// CountTracks reports how many songs have cached metadata.
func CountTracks(ctx context.Context, database *sql.DB) (int64, error) {
	return countQuery(ctx, database, "SELECT COUNT(*) FROM tracks", "count metadata tracks")
}

// CountDistinctTrackNames reports how many individual, distinct track names are
// cached (case-insensitively), counting a song once no matter how many records
// represent it.
func CountDistinctTrackNames(ctx context.Context, database *sql.DB) (int64, error) {
	return countQuery(ctx, database, "SELECT COUNT(DISTINCT name_lower) FROM tracks", "count distinct track names")
}

// CountLyricsTracks reports how many songs have cached lyrics. Mindful of
// content deduplication in the lyrics table, this counts the song-to-lyrics
// associations rather than the deduplicated content rows.
func CountLyricsTracks(ctx context.Context, database *sql.DB) (int64, error) {
	return countQuery(ctx, database, "SELECT COUNT(*) FROM lyrics_tracks", "count lyrics tracks")
}

// CountCovers reports how many cached cover entries exist: songs whose metadata
// cache carries a cover URL, plus album and artist covers stored in the cover
// URL cache. Negative cache rows (a checked miss with an empty URL) are not
// counted. A nil coverDB leaves the album and artist counts at zero.
func CountCovers(ctx context.Context, metadataDB, coverDB *sql.DB) (CoverCounts, error) {
	if metadataDB == nil && coverDB == nil {
		return CoverCounts{}, fmt.Errorf("count covers: %w", errors.New("database is nil"))
	}
	var counts CoverCounts
	var err error
	if metadataDB != nil {
		if counts.Songs, err = countQuery(ctx, metadataDB, `SELECT COUNT(*) FROM tracks WHERE cover_url IS NOT NULL AND cover_url <> ''`, "count song covers"); err != nil {
			return CoverCounts{}, err
		}
	}
	if coverDB != nil {
		if counts.Albums, err = countQuery(ctx, coverDB, `SELECT COUNT(*) FROM cover_urls WHERE entity_type = 'album' AND cover_url IS NOT NULL AND cover_url <> ''`, "count album covers"); err != nil {
			return CoverCounts{}, err
		}
		if counts.Artists, err = countQuery(ctx, coverDB, `SELECT COUNT(*) FROM cover_urls WHERE entity_type = 'artist' AND cover_url IS NOT NULL AND cover_url <> ''`, "count artist covers"); err != nil {
			return CoverCounts{}, err
		}
	}
	return counts, nil
}

func countQuery(ctx context.Context, database *sql.DB, statement, label string) (int64, error) {
	if database == nil {
		return 0, fmt.Errorf("%s: %w", label, errors.New("database is nil"))
	}
	var count int64
	if err := database.QueryRowContext(ctx, statement).Scan(&count); err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	return count, nil
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

func richContentHash(content, format, syncType string) string {
	hash := sha256.Sum256([]byte(format + "\x00" + syncType + "\x00" + content))
	return hex.EncodeToString(hash[:])
}
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
