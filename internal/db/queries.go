package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
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
//
// metadata_checked_at is restamped whenever the caller reports a definitive
// answer (MetadataChecked), and only then. It answers "when did a provider last
// settle this track", so an upsert that carries no upstream result — a new set
// of lyrics, a corrected duration — must leave it alone or every negative would
// age back to fresh whenever the request path touched the row.
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
metadata_checked_at=CASE WHEN ? THEN CURRENT_TIMESTAMP ELSE metadata_checked_at END,
lyrics_checked=MAX(lyrics_checked, ?),
lyrics_checked_at=CASE WHEN ? THEN CURRENT_TIMESTAMP ELSE lyrics_checked_at END,
updated_at=CURRENT_TIMESTAMP, source=? WHERE id=?`,
				track.Name, track.NameLower, track.ArtistName, track.ArtistNameLower, track.AlbumName, track.AlbumNameLower,
				track.Duration, track.Duration, track.Genre, track.Genre, track.GenreLower, track.GenreLower, track.Year, track.Year,
				track.ReleaseDate, track.ReleaseDate, track.ISRC, track.ISRC, track.MusicBrainzReleaseID, track.MusicBrainzReleaseID,
				track.MusicBrainzReleaseGroupID, track.MusicBrainzReleaseGroupID, track.MusicBrainzArtistID, track.MusicBrainzArtistID,
				track.CoverURL, track.CoverURL, track.MetadataSource, track.MetadataSource, track.CoverURLSource, track.CoverURLSource,
				track.MetadataChecked, track.CoverURLChecked, track.MetadataChecked,
				track.LyricsChecked, track.LyricsChecked, track.Source, existingID)
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
metadata_checked,metadata_checked_at,lyrics_checked,lyrics_checked_at,cover_url_checked,source) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,CASE WHEN ? THEN CURRENT_TIMESTAMP ELSE NULL END,?,CASE WHEN ? THEN CURRENT_TIMESTAMP ELSE NULL END,?,?)
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
metadata_checked_at=CASE WHEN excluded.metadata_checked THEN CURRENT_TIMESTAMP ELSE tracks.metadata_checked_at END,
lyrics_checked=MAX(tracks.lyrics_checked,excluded.lyrics_checked),
lyrics_checked_at=CASE WHEN excluded.lyrics_checked THEN CURRENT_TIMESTAMP ELSE tracks.lyrics_checked_at END,
updated_at=CURRENT_TIMESTAMP, source=excluded.source RETURNING id`
	args := []any{track.Name, track.NameLower, track.ArtistName, track.ArtistNameLower, track.AlbumName, track.AlbumNameLower, track.Duration,
		nullableText(track.Genre), nullableText(track.GenreLower), track.Year, nullableText(track.ReleaseDate), nullableText(track.ISRC),
		nullableText(track.MusicBrainzRecordingID), nullableText(track.MusicBrainzReleaseID), nullableText(track.MusicBrainzReleaseGroupID), nullableText(track.MusicBrainzArtistID),
		nullableText(track.CoverURL), nullableText(track.MetadataSource), nullableText(track.CoverURLSource), track.MetadataChecked, track.MetadataChecked, track.LyricsChecked, track.LyricsChecked, track.CoverURLChecked, track.Source}
	if err := database.QueryRowContext(ctx, statement, args...).Scan(&track.ID); err != nil {
		return 0, fmt.Errorf("upsert metadata: %w", err)
	}
	return track.ID, nil
}

// prepareLyricsRow fills in the values a lyrics row can be stored with but the
// caller may not have computed: the content hash, and the two availability flags
// that the has_* columns are always read as. It is shared by every write path
// because TrackIDsWithUsableLyrics and the live request path disagree unless the
// flags and the text stay in step.
func prepareLyricsRow(lyrics Lyrics) Lyrics {
	if lyrics.ContentHash == "" {
		lyrics.ContentHash = contentHash(lyrics.PlainLyrics, lyrics.SyncedLyrics)
	}
	lyrics.HasPlain = lyrics.HasPlain || lyrics.PlainLyrics != ""
	lyrics.HasSynced = lyrics.HasSynced || lyrics.SyncedLyrics != ""
	return lyrics
}

// insertLyricsRow stores one lyrics row and its track association inside the
// caller's transaction, returning the row's id.
//
// The lyrics table is content-addressed on content_hash, so a payload already on
// disk resolves to the existing row rather than a duplicate. That dedup is
// global, not per provider: two providers returning identical text share one row,
// and the second INSERT OR IGNORE reports no row, which is why the lookup below
// exists. It also means lyrics.source records whichever provider wrote first, so
// source is provenance for a row that can hold more than one answer, not a
// guarantee that every provider appears in it.
func insertLyricsRow(ctx context.Context, tx *sql.Tx, trackID int64, lyrics Lyrics) (int64, error) {
	lyrics = prepareLyricsRow(lyrics)
	var lyricsID int64
	err := tx.QueryRowContext(ctx, `INSERT OR IGNORE INTO lyrics (track_id,plain_lyrics,synced_lyrics,has_plain_lyrics,has_synced_lyrics,instrumental,content_hash,source) VALUES (?,?,?,?,?,?,?,?) RETURNING id`, trackID, nullableText(lyrics.PlainLyrics), nullableText(lyrics.SyncedLyrics), lyrics.HasPlain, lyrics.HasSynced, lyrics.Instrumental, lyrics.ContentHash, lyrics.Source).Scan(&lyricsID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("insert lyrics: %w", err)
		}
		if err = tx.QueryRowContext(ctx, `SELECT id FROM lyrics WHERE content_hash=? LIMIT 1`, lyrics.ContentHash).Scan(&lyricsID); err != nil {
			return 0, fmt.Errorf("select lyrics: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO lyrics_tracks(track_id,lyrics_id) VALUES(?,?)`, trackID, lyricsID); err != nil {
		return 0, fmt.Errorf("associate lyrics: %w", err)
	}
	return lyricsID, nil
}

// InsertTrackWithLyrics writes metadata and lyrics to their respective databases.
// Metadata remains valid if the separate lyrics write fails.
//
// A lyrics row in hand is a definitive upstream answer, so the track is stamped
// lyrics-checked here. That is what lets a later refresh judge the track's age:
// without it, every track the request path ever served would still read as
// never-settled and the lyrics backfill would re-fetch the whole library.
func InsertTrackWithLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB, track Track, lyrics Lyrics) (trackID, lyricsID int64, err error) {
	if metadataDB == nil || lyricsDB == nil {
		return 0, 0, errors.New("metadata and lyrics databases are required")
	}
	if track.Source == "" {
		track.Source = "local"
	}
	track.LyricsChecked = true
	normalizeTrack(&track)
	if lyrics.Source == "" {
		lyrics.Source = track.Source
	}
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
	if lyricsID, err = insertLyricsRow(ctx, lyricsTx, trackID, lyrics); err != nil {
		return trackID, 0, err
	}
	if err = lyricsTx.Commit(); err != nil {
		return trackID, 0, fmt.Errorf("commit lyrics: %w", err)
	}
	if _, err = metadataDB.ExecContext(ctx, `UPDATE tracks SET last_lyrics_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, lyricsID, trackID); err != nil {
		return trackID, lyricsID, fmt.Errorf("link lyrics reference: %w", err)
	}
	return trackID, lyricsID, nil
}

// StoreLyricsVariants stores every provider's answer for one track in a single
// lyrics transaction, and points the track at exactly one of them.
//
// It exists because the batch job asks all providers at once: the schema already
// allows a track many lyrics rows, but only one of them is served, and
// InsertTrackWithLyrics moved that pointer on every call. Writing the variants
// through it in a loop would leave the pointer on whichever provider was written
// last, which under a concurrent fan-out is not even stable between runs. So the
// winner is written first, the alternatives in the same transaction, and the
// pointer is set once at the end.
//
// The transaction spans all the rows, so a track is never left with some of its
// provider answers and not others. It cannot span both databases, because SQLite
// has no two-database transaction, so the order is still metadata-then-pointer
// and a crash in between leaves a track with lyrics and no flag, which the next
// run re-settles.
func StoreLyricsVariants(ctx context.Context, metadataDB, lyricsDB *sql.DB, track Track, winner Lyrics, variants []Lyrics) (trackID, lyricsID int64, err error) {
	if metadataDB == nil || lyricsDB == nil {
		return 0, 0, errors.New("metadata and lyrics databases are required")
	}
	if track.Source == "" {
		track.Source = "local"
	}
	track.LyricsChecked = true
	normalizeTrack(&track)
	if winner.Source == "" {
		winner.Source = track.Source
	}
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
	if lyricsID, err = insertLyricsRow(ctx, lyricsTx, trackID, winner); err != nil {
		return trackID, 0, err
	}
	for _, variant := range variants {
		if _, err = insertLyricsRow(ctx, lyricsTx, trackID, variant); err != nil {
			return trackID, 0, err
		}
	}
	if err = lyricsTx.Commit(); err != nil {
		return trackID, 0, fmt.Errorf("commit lyrics: %w", err)
	}
	if _, err = metadataDB.ExecContext(ctx, `UPDATE tracks SET last_lyrics_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, lyricsID, trackID); err != nil {
		return trackID, lyricsID, fmt.Errorf("link lyrics reference: %w", err)
	}
	return trackID, lyricsID, nil
}

// StoreLyricsAnswer stores one provider's answer for a track and links the row to
// it, returning the row's id.
//
// It deliberately moves neither the served pointer nor the settle flag, because a
// batch run now asks one provider at a time and a song is not answered until all
// of them have replied. Both of those decisions belong to PointTrackAtBestLyrics,
// which can compare this answer against the ones already on disk rather than
// against a fan-out that no longer exists.
//
// The track row is not upserted: the caller works from an existing row whose
// identity is already correct, so re-writing the lowercased keys here could only
// risk re-keying it. Unlike InsertTrackWithLyrics this also cannot be the path
// that creates a track, which is right because only the request path does that.
func StoreLyricsAnswer(ctx context.Context, lyricsDB *sql.DB, trackID int64, lyrics Lyrics) (int64, error) {
	if lyricsDB == nil {
		return 0, errors.New("lyrics database is nil")
	}
	if trackID <= 0 {
		return 0, errors.New("track is required")
	}
	tx, err := lyricsDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin lyrics insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lyricsID, err := insertLyricsRow(ctx, tx, trackID, lyrics)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit lyrics: %w", err)
	}
	return lyricsID, nil
}

// lyricsQualityTier ranks a stored row for the purpose of choosing which one a
// track should serve.
//
// It is the same tiering the live fan-out and the batch job apply in memory, read
// back out of the has_* columns because that is all a stored row keeps. Synced
// beats plain, and plain and instrumental are one tier: neither carries timing.
func lyricsQualityTier(hasSynced, hasPlainOrInstrumental bool) int {
	switch {
	case hasSynced:
		return 2
	case hasPlainOrInstrumental:
		return 1
	default:
		return 0
	}
}

// PointTrackAtBestLyrics points a track at the best of the given stored rows,
// returning the id it now serves, or 0 when the pointer was left alone.
//
// The pointer moves only when the track serves nothing or the candidate is a
// strict improvement. Both halves matter for a run that resumes: an earlier run
// may already have stored synced lyrics from one provider, and a later one that
// only draws plain answers from another must not replace them; and an equally good
// answer must not displace an equally good one either, or every answer in a song
// would shuffle the pointer as it arrived and a refresh run would keep flipping it.
//
// An empty candidate list is not an error. It is how a run reports that the
// provider it asked had nothing, and the track is left pointing wherever it
// already pointed, which for a track with no lyrics at all is nowhere.
func PointTrackAtBestLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB, trackID int64, candidateIDs []int64) (int64, error) {
	if metadataDB == nil || lyricsDB == nil {
		return 0, errors.New("metadata and lyrics databases are required")
	}
	if trackID <= 0 {
		return 0, errors.New("track is required")
	}
	best, bestTier, err := bestLyricsCandidate(ctx, lyricsDB, trackID, candidateIDs)
	if err != nil || best == 0 {
		return 0, err
	}
	current, currentTier, err := pointedLyrics(ctx, metadataDB, lyricsDB, trackID)
	if err != nil {
		return 0, err
	}
	if current > 0 && currentTier >= bestTier {
		return 0, nil
	}
	if _, err := metadataDB.ExecContext(ctx,
		`UPDATE tracks SET last_lyrics_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, best, trackID); err != nil {
		return 0, fmt.Errorf("link lyrics reference: %w", err)
	}
	return best, nil
}

// bestLyricsCandidate returns the highest-ranked candidate id and its tier.
//
// Candidates are read with one query so the comparison is against the rows as
// they are actually stored, not against what the caller believes it wrote.
func bestLyricsCandidate(ctx context.Context, lyricsDB *sql.DB, trackID int64, candidateIDs []int64) (int64, int, error) {
	ids := make([]int64, 0, len(candidateIDs))
	seen := make(map[int64]bool, len(candidateIDs))
	for _, id := range candidateIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, 0, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := lyricsDB.QueryContext(ctx,
		`SELECT id, has_synced_lyrics, (has_plain_lyrics OR instrumental) FROM lyrics WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return 0, 0, fmt.Errorf("read stored lyrics: %w", err)
	}
	var best int64
	bestTier := 0
	scanErr := func() error {
		defer rows.Close()
		for rows.Next() {
			var id int64
			var hasSynced, hasPlainOrInstrumental bool
			if err := rows.Scan(&id, &hasSynced, &hasPlainOrInstrumental); err != nil {
				return fmt.Errorf("scan stored lyrics: %w", err)
			}
			if tier := lyricsQualityTier(hasSynced, hasPlainOrInstrumental); tier > bestTier {
				best, bestTier = id, tier
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate stored lyrics: %w", err)
		}
		return nil
	}()
	if scanErr != nil {
		return 0, 0, scanErr
	}
	// A candidate that resolves to a row with nothing in it is not worth serving,
	// so it is treated as absent rather than as a tier-zero improvement.
	if bestTier == 0 {
		return 0, 0, nil
	}
	// Only rows actually associated with this track may be served for it, so a
	// caller cannot point one track at another track's lyrics by passing its id.
	//
	// The candidate scan above is closed before this runs. A handle opened with
	// MaxOpenConns of 1 hands its connection back only when the rows are closed, so
	// querying again with the cursor still open would wait on the connection the
	// cursor is holding.
	var linked bool
	if err := lyricsDB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM lyrics_tracks WHERE track_id=? AND lyrics_id=?)`, trackID, best).Scan(&linked); err != nil {
		return 0, 0, fmt.Errorf("check lyrics association: %w", err)
	}
	if !linked {
		return 0, 0, nil
	}
	return best, bestTier, nil
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
	rows, err := database.QueryContext(ctx, `SELECT id,content,format,sync_type FROM lyrics_sync_variants WHERE format='ttml' COLLATE NOCASE`)
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
WHERE id=? AND format='ttml' COLLATE NOCASE`, content, format, richContentHash(content, format, row.syncType), row.id)
		if err != nil {
			return migrated, fmt.Errorf("migrate rich lyrics %d: %w", row.id, err)
		}
		if affected, err := result.RowsAffected(); err == nil && affected > 0 {
			migrated++
		}
	}
	return migrated, nil
}

// ProviderFetchStaleTTL is how long a failed provider fetch is remembered.
const ProviderFetchStaleTTL = 24 * time.Hour

// UpsertProviderFetch records that provider was tried for trackID.
func UpsertProviderFetch(ctx context.Context, database *sql.DB, trackID int64, provider string, success bool) error {
	if database == nil {
		return errors.New("lyrics database is nil")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if trackID <= 0 || provider == "" {
		return errors.New("track and provider are required")
	}
	_, err := database.ExecContext(ctx, `INSERT INTO lyrics_provider_fetches (track_id, provider, last_fetched_at, last_success)
VALUES (?, ?, CURRENT_TIMESTAMP, ?)
ON CONFLICT(track_id, provider) DO UPDATE SET last_fetched_at=CURRENT_TIMESTAMP, last_success=excluded.last_success`, trackID, provider, success)
	if err != nil {
		return fmt.Errorf("upsert provider fetch: %w", err)
	}
	return nil
}

// HasRecentProviderFetch reports whether provider was tried for trackID within ttl.
func HasRecentProviderFetch(ctx context.Context, database *sql.DB, trackID int64, provider string, ttl time.Duration) (bool, error) {
	if database == nil {
		return false, errors.New("lyrics database is nil")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if trackID <= 0 || provider == "" {
		return false, nil
	}
	var last string
	err := database.QueryRowContext(ctx, `SELECT last_fetched_at FROM lyrics_provider_fetches WHERE track_id=? AND provider=?`, trackID, provider).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check provider fetch: %w", err)
	}
	// Parse as stored CURRENT_TIMESTAMP (UTC, "2006-01-02 15:04:05").
	t, err := time.Parse("2006-01-02 15:04:05", last)
	if err != nil {
		// Fallback: try RFC3339.
		t, _ = time.Parse(time.RFC3339, last)
	}
	return time.Since(t) < ttl, nil
}

// ListRichLyricsSources returns every (source,sync_type) pair cached for a track.
// ListRecentProviderFetches returns every provider tried for trackID within
// ttl, mapped to whether the last attempt succeeded. The request-path fan-out
// uses it to skip providers recently answered, positive or negative.
func ListRecentProviderFetches(ctx context.Context, database *sql.DB, trackID int64, ttl time.Duration) (map[string]bool, error) {
	if database == nil {
		return nil, errors.New("lyrics database is nil")
	}
	if trackID <= 0 {
		return map[string]bool{}, nil
	}
	rows, err := database.QueryContext(ctx, `SELECT provider, last_fetched_at, last_success FROM lyrics_provider_fetches WHERE track_id=?`, trackID)
	if err != nil {
		return nil, fmt.Errorf("list provider fetches: %w", err)
	}
	defer rows.Close()
	recent := make(map[string]bool)
	for rows.Next() {
		var provider, last string
		var success bool
		if err := rows.Scan(&provider, &last, &success); err != nil {
			return nil, fmt.Errorf("scan provider fetch: %w", err)
		}
		t, err := time.Parse("2006-01-02 15:04:05", last)
		if err != nil {
			t, _ = time.Parse(time.RFC3339, last)
		}
		if time.Since(t) >= ttl {
			continue
		}
		recent[strings.ToLower(strings.TrimSpace(provider))] = success
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider fetches: %w", err)
	}
	return recent, nil
}

// BestStoredLyrics returns the id and tier of the best usable lyrics row linked
// to a track, or 0 and 0 when the track has none.
//
// It searches the track's whole history rather than the rows one run just wrote,
// because a track's lyrics arrive from more places than a batch job: earlier
// passes of this one, and the live request path. Settling against the full set is
// what lets a run that resumed pick up the better answer an earlier run stored.
//
// The tie-break is the lowest id, which is the oldest row. It is arbitrary but it
// is not arrival order, so re-settling the same track twice in a row picks the same
// answer both times.
func BestStoredLyrics(ctx context.Context, lyricsDB *sql.DB, trackID int64) (int64, int, error) {
	if lyricsDB == nil {
		return 0, 0, errors.New("lyrics database is nil")
	}
	if trackID <= 0 {
		return 0, 0, nil
	}
	var id int64
	var hasSynced, hasPlainOrInstrumental bool
	err := lyricsDB.QueryRowContext(ctx,
		`SELECT l.id, l.has_synced_lyrics, (l.has_plain_lyrics OR l.instrumental)
FROM lyrics_tracks AS lt JOIN lyrics AS l ON l.id = lt.lyrics_id
WHERE lt.track_id = ?
AND (l.has_plain_lyrics OR l.has_synced_lyrics OR l.instrumental)
ORDER BY CASE WHEN l.has_synced_lyrics THEN 2
              WHEN l.has_plain_lyrics OR l.instrumental THEN 1
              ELSE 0 END DESC, l.id
LIMIT 1`, trackID).Scan(&id, &hasSynced, &hasPlainOrInstrumental)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read best stored lyrics: %w", err)
	}
	return id, lyricsQualityTier(hasSynced, hasPlainOrInstrumental), nil
}

// SettleTrackLyrics records that a lyrics provider answered for a track and points
// the track at the best lyrics stored for it, in one metadata transaction.
//
// It is the last step of a batch lookup, and it runs only once every provider has
// given a definitive answer. That is what makes the flag worth having: it says
// the run checked everywhere rather than that one upstream happened to answer.
//
// A track with nothing stored is settled with no pointer. "Every provider says
// this song has no lyrics" is a real answer and the run needs to record it, or
// every pass would ask the same question again forever.
//
// The pointer moves only when the track serves nothing or the best stored row is
// a strict improvement, which is the same rule PointTrackAtBestLyrics applies per
// answer. A song that already serves synced lyrics is not replaced by the plain
// answer a later run happened to draw, and two equally good answers do not shuffle
// the pointer back and forth.
func SettleTrackLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB, trackID int64) error {
	if metadataDB == nil || lyricsDB == nil {
		return errors.New("metadata and lyrics databases are required")
	}
	if trackID <= 0 {
		return errors.New("track is required")
	}
	best, bestTier, err := BestStoredLyrics(ctx, lyricsDB, trackID)
	if err != nil {
		return err
	}
	// The pointer and the row it names live in different files, so the comparison
	// needs a read outside the transaction below. A handle opened with
	// MaxOpenConns of 1 hands its connection to that transaction and takes it back
	// only at commit, so reading here rather than inside is not a style preference:
	// the other order waits on the connection the transaction is holding.
	//
	// It leaves a narrow window in which the request path could move the pointer to
	// something better in between, and then this write wins a race it should have
	// lost. The request path sets the pointer unconditionally too, so the two are
	// already last-writer-wins and neither is relying on the other to be careful.
	current := int64(0)
	if best > 0 {
		var currentTier int
		current, currentTier, err = pointedLyrics(ctx, metadataDB, lyricsDB, trackID)
		if err != nil {
			return err
		}
		if current > 0 && currentTier >= bestTier {
			best = 0
		}
	}

	tx, err := metadataDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin lyrics settle: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if best > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tracks SET last_lyrics_id=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, best, trackID); err != nil {
			return fmt.Errorf("link lyrics reference: %w", err)
		}
	}
	if err := MarkTrackLyricsChecked(ctx, tx, trackID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit lyrics settle: %w", err)
	}
	return nil
}

// pointedLyrics reports the id and tier of the row a track already serves, or 0
// and 0 when it serves nothing.
//
// The two halves come from two databases and cannot be joined: the pointer lives
// in the metadata file and the lyrics rows in the other one. That is why every
// function needing both takes both handles rather than one connection that could
// have done it in a single statement.
func pointedLyrics(ctx context.Context, metadataDB, lyricsDB *sql.DB, trackID int64) (int64, int, error) {
	// last_lyrics_id is nullable: a track nobody has found lyrics for has no row
	// to point at, and COALESCE reads that as zero rather than failing the scan.
	var pointed int64
	if err := metadataDB.QueryRowContext(ctx,
		"SELECT COALESCE(last_lyrics_id, 0) FROM tracks WHERE id=?", trackID).Scan(&pointed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("read current lyrics pointer: %w", err)
	}
	if pointed <= 0 {
		return 0, 0, nil
	}
	tier, err := storedLyricsTier(ctx, lyricsDB, pointed)
	if err != nil {
		return 0, 0, err
	}
	return pointed, tier, nil
}

// storedLyricsTier reports the tier of one stored lyrics row, or 0 when the row is
// gone.
func storedLyricsTier(ctx context.Context, lyricsDB *sql.DB, lyricsID int64) (int, error) {
	var hasSynced, hasPlainOrInstrumental bool
	if err := lyricsDB.QueryRowContext(ctx,
		"SELECT has_synced_lyrics, (has_plain_lyrics OR instrumental) FROM lyrics WHERE id=?", lyricsID).
		Scan(&hasSynced, &hasPlainOrInstrumental); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A pointer at a row that is gone is treated as serving nothing, so a
			// settle repairs it rather than leaving the track unserveable.
			return 0, nil
		}
		return 0, fmt.Errorf("read current lyrics row: %w", err)
	}
	return lyricsQualityTier(hasSynced, hasPlainOrInstrumental), nil
}

// ListProviderFetchesForTracks returns, for each of the given tracks, every
// provider already asked about it and whether that last attempt succeeded.
//
// Unlike ListRecentProviderFetches this applies no freshness window. It exists so
// the lyrics backfill can resume a track a previous run left partway: a track
// still flagged as unsettled that already has ledger rows is by definition an
// interrupted run, so its rows describe real work that does not need repeating,
// however long ago they were written. Each re-ask refreshes last_fetched_at, so
// the request path's own skip window stays correct.
func ListProviderFetchesForTracks(ctx context.Context, database *sql.DB, trackIDs []int64) (map[int64]map[string]bool, error) {
	fetches := make(map[int64]map[string]bool)
	if database == nil || len(trackIDs) == 0 {
		return fetches, nil
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
		return fetches, nil
	}
	rows, err := database.QueryContext(ctx,
		`SELECT track_id, provider, last_success FROM lyrics_provider_fetches WHERE track_id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("list provider fetches for tracks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var trackID int64
		var provider string
		var success bool
		if err := rows.Scan(&trackID, &provider, &success); err != nil {
			return nil, fmt.Errorf("scan provider fetch for track: %w", err)
		}
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if fetches[trackID] == nil {
			fetches[trackID] = make(map[string]bool)
		}
		fetches[trackID][name] = success
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider fetches for tracks: %w", err)
	}
	return fetches, nil
}

func ListRichLyricsSources(ctx context.Context, database *sql.DB, trackID int64) (map[string]struct{}, error) {
	if database == nil {
		return nil, errors.New("lyrics database is nil")
	}
	rows, err := database.QueryContext(ctx, `SELECT source, sync_type FROM lyrics_sync_variants WHERE track_id=?`, trackID)
	if err != nil {
		return nil, fmt.Errorf("list rich lyrics sources: %w", err)
	}
	defer rows.Close()
	sources := make(map[string]struct{})
	for rows.Next() {
		var source, syncType string
		if err := rows.Scan(&source, &syncType); err != nil {
			return nil, fmt.Errorf("scan rich lyrics source: %w", err)
		}
		key := strings.ToLower(strings.TrimSpace(source)) + "\x00" + strings.ToLower(strings.TrimSpace(syncType))
		sources[key] = struct{}{}
		// Also index by source alone for quick existence check.
		sources[strings.ToLower(strings.TrimSpace(source))] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rich lyrics sources: %w", err)
	}
	return sources, nil
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

// FindRichLyricsByName returns the best cached rich payload for any track
// sharing the normalized track and artist name. This is used as a generic
// cross-album fallback when a specific track_id has no rich variant but a
// sibling row for the same song does. Artist must match; album and duration
// are intentionally ignored. It uses metadataDB to resolve candidate track
// ids and lyricsDB to fetch the variant, because the two databases are
// separate SQLite files.
func FindRichLyricsByName(ctx context.Context, metadataDB, lyricsDB *sql.DB, trackName, artistName, syncType string) (*RichLyrics, error) {
	if lyricsDB == nil {
		return nil, errors.New("lyrics database is nil")
	}
	if metadataDB == nil {
		return nil, errors.New("metadata database is nil")
	}
	trackName = strings.TrimSpace(trackName)
	artistName = strings.TrimSpace(artistName)
	if trackName == "" || artistName == "" {
		return nil, sql.ErrNoRows
	}
	syncType = strings.ToLower(strings.TrimSpace(syncType))
	nameLower := normalize(trackName)
	artistLower := normalize(artistName)
	rows, err := metadataDB.QueryContext(ctx, `SELECT id FROM tracks WHERE name_lower=? AND artist_name_lower=?`, nameLower, artistLower)
	if err != nil {
		return nil, fmt.Errorf("find rich lyrics by name: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("find rich lyrics by name: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find rich lyrics by name: %w", err)
	}
	if len(ids) == 0 {
		return nil, sql.ErrNoRows
	}
	query := `SELECT id,track_id,content,format,sync_type,source,content_hash
FROM lyrics_sync_variants
WHERE track_id IN (` + strings.Repeat("?,", len(ids)-1) + `?) AND (?='' OR sync_type=?)
ORDER BY CASE sync_type WHEN 'word' THEN 0 WHEN 'syllable' THEN 1 WHEN 'richsync' THEN 2 ELSE 3 END,
         CASE source WHEN 'unison' THEN 0 ELSE 1 END, id DESC LIMIT 1`
	args := make([]any, 0, len(ids)+2)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, syncType, syncType)
	rich := &RichLyrics{}
	err = lyricsDB.QueryRowContext(ctx, query, args...).Scan(&rich.ID, &rich.TrackID, &rich.Content, &rich.Format, &rich.SyncType, &rich.Source, &rich.Hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("find rich lyrics by name: %w", err)
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
	err := database.QueryRowContext(ctx, "SELECT id,entity_type,COALESCE(artist_name_lower,''),COALESCE(album_name_lower,''),COALESCE(cover_url,''),COALESCE(cover_source,''),COALESCE(checked_at,'') FROM cover_urls WHERE entity_type=? AND artist_name_lower=? AND album_name_lower=? LIMIT 1",
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
//
// Distinct tracks, though: a track can hold several rows at once, because a batch
// run that asks every provider stores all of their answers and serves the best one
// (see StoreLyricsVariants). Counting associations would report that as six songs
// where there is one, so the count is collapsed to the tracks themselves.
func CountLyricsTracks(ctx context.Context, database *sql.DB) (int64, error) {
	return countQuery(ctx, database, "SELECT COUNT(DISTINCT track_id) FROM lyrics_tracks", "count lyrics tracks")
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
