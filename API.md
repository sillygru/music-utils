# music-utils API

All endpoints are served on `:8080` by default and return JSON.

- Errors: `{"code": <http_status>, "message": "<description>"}`.
- `/api/*` endpoints are rate limited per client IP.
- Metadata and lyrics exact lookups are local-first, using separate SQLite databases.
- The old `/api/get` and `/api/search` paths do not exist.

## Metadata object

`GET /api/metadata/get` and `GET /api/metadata/search` return objects with:

| Field | Type | Description |
| --- | --- | --- |
| `id` | integer | Local SQLite track ID. |
| `trackName` | string | Song title. |
| `artistName` | string | Artist name. |
| `albumName` | string | Album/release title. |
| `duration` | number | Seconds. |
| `genre` | string | Best available genre/tag. |
| `year` | integer | Release year. |
| `releaseDate` | string | Provider release date, when available. |
| `isrc` | string | ISRC, when available. |
| `musicbrainzRecordingId` | string | Canonical recording MBID. |
| `musicbrainzReleaseId` | string | Selected release MBID. |
| `musicbrainzReleaseGroupId` | string | Release-group MBID. |
| `musicbrainzArtistId` | string | Primary artist MBID. |
| `coverUrl` | string | Cached front-cover URL, when available. |
| `metadataSource` | string | Metadata provenance, currently `musicbrainz`. |
| `coverUrlSource` | string | Artwork provenance, currently `cover_art_archive`. |

## `GET /api/metadata/get`

Exact song lookup. Checks SQLite first. If missing or not enriched and
`METADATA_FALLBACK_ENABLED=true`, searches MusicBrainz, resolves Cover Art
Archive artwork, stores the result, and returns the cached record.

Query parameters:

- `track_name` — required.
- `artist_name` — required.
- `album_name` — optional narrowing hint.
- `duration` — optional non-negative seconds hint.

Example:

```sh
curl 'http://localhost:8080/api/metadata/get?track_name=Example%20Song&artist_name=Example%20Artist'
```

Responses: `200` metadata object, `400` invalid/missing input, `404` provider
miss or disabled fallback, `429` rate limited, `500` internal error.

## `GET /api/metadata/search`

Searches only the local FTS5 catalog. It never calls an upstream API.

Query parameters:

- `q`, or one or more of `track_name`, `artist_name`, `album_name`, `genre`.
- `limit`, default `20`, range `1–50`.

```sh
curl 'http://localhost:8080/api/metadata/search?q=example&limit=20'
```

## `GET /api/cover/get`

Returns only cached artwork; it never calls upstream providers.

Query parameters: required `track_name` and `artist_name`; optional
`album_name`.

Example response:

```json
{
  "id": 1,
  "trackName": "Example Song",
  "artistName": "Example Artist",
  "albumName": "Example Album",
  "coverUrl": "https://coverartarchive.org/...",
  "coverUrlSource": "cover_art_archive"
}
```

Responses: `200` cached cover, `400` invalid input, `404` not cached/not found,
`429` rate limited, `500` internal error.

## `GET /api/lyrics/get`

Exact lyrics lookup. Checks SQLite first; on a miss and when
`LRCLIB_FALLBACK_ENABLED=true`, fetches LRCLIB and caches the result.

Parameters: required `track_name` and `artist_name`; optional `album_name` and
non-negative `duration`.

Returns the lyrics fields `instrumental`, `plainLyrics`, and `syncedLyrics`,
plus the track title, artist, album, duration, and local ID.

```sh
curl 'http://localhost:8080/api/lyrics/get?track_name=Example%20Song&artist_name=Example%20Artist'
```

## `GET /api/lyrics/search`

Searches the local FTS5 catalog and never calls LRCLIB.

Parameters: `q`, or one or more of `track_name`, `artist_name`, `album_name`;
optional `limit` from `1–50`, default `20`.

```sh
curl 'http://localhost:8080/api/lyrics/search?q=example&limit=20'
```

## Health/version

- `GET /healthz` → `{"status":"ok"}`; not rate limited.
- `GET /version` → `{"version":"v0.2.0"}`; not rate limited.

## Database layout

`METADATA_DB_PATH` stores tracks, metadata, cover URLs, provenance, and metadata
FTS5. `LYRICS_DB_PATH` stores lyrics bodies and their track associations. The
service composes responses in Go; it does not use cross-database SQL joins.
For an existing combined database, run the service once with that file as the
metadata path and a new lyrics path to copy lyrics and remove the old combined
lyrics tables safely.

## Provider and cache behavior

MusicBrainz and Cover Art Archive are queried with a descriptive User-Agent
and the configured `RATE_LIMIT_PER_SEC` burst plus `RATE_LIMIT_PER_MIN` rolling
window. Identical concurrent metadata lookups share one in-flight operation. Provider
responses are bounded and cached transactionally in SQLite. Search endpoints
are local-only to keep upstream traffic predictable.

LRCLIB remains the lyrics provider and uses its existing bounded client,
User-Agent, timeout, and cache behavior.

## Rate limiting

`RATE_LIMIT_PER_SEC` controls burst/token-bucket rate and
`RATE_LIMIT_PER_MIN` controls the rolling-minute cap. Rate-limited responses
include `Retry-After`.
