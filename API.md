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
| `musicbrainzRecordingId` | string | Canonical recording MBID (legacy, when populated). |
| `musicbrainzReleaseId` | string | Selected release MBID (legacy, when populated). |
| `musicbrainzReleaseGroupId` | string | Release-group MBID (legacy, when populated). |
| `musicbrainzArtistId` | string | Primary artist MBID (legacy, when populated). |
| `coverUrl` | string | Cover URL returned by a provider, when available. |
| `metadataSource` | string | Metadata provenance: `itunes`, `deezer`, or user-provided. |
| `coverUrlSource` | string | Artwork provenance: `itunes` or `deezer`, when a cover URL was provided. |

## `GET /api/metadata/get`

Exact song lookup. Checks SQLite first. If missing or not enriched and
`METADATA_FALLBACK_ENABLED=true`, tries the metadata provider chain (iTunes,
then Deezer) in order, stores the result, and returns the cached record. The
dedicated Cover Art Archive call was removed; a cover URL is only kept when a
provider includes one in its response for free.

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
  "coverUrl": "https://is1-ssl.mzstatic.com/.../600x600bb.jpg",
  "coverUrlSource": "itunes"
}
```

Responses: `200` cached cover, `400` invalid input, `404` not cached/not found,
`429` rate limited, `500` internal error.

## `GET /api/cover/artist`

Returns artist artwork as a cover URL. It is an enrichment endpoint (unlike the
cache-only `/api/cover/get`): on a miss it resolves the cover provider chain
(Last.fm → iTunes → Deezer) in order, caches the winning URL, and returns it.
The result is persisted in a dedicated cover database.

Query parameter: required `artist_name`.

Example response:

```json
{
  "id": 1,
  "entityType": "artist",
  "artistName": "Radiohead",
  "coverUrl": "https://is1-ssl.mzstatic.com/.../600x600bb.jpg",
  "coverUrlSource": "itunes"
}
```

Responses: `200` cover URL, `400` invalid input, `404` provider miss (negative
cached for the cache window), `429` rate limited, `500` internal error.

## `GET /api/cover/album`

Returns album artwork as a cover URL, with the same enrichment and persistence
behavior as `/api/cover/artist`.

Query parameters: required `artist_name` and `album_name`.

Example response:

```json
{
  "id": 1,
  "entityType": "album",
  "artistName": "Radiohead",
  "albumName": "OK Computer",
  "coverUrl": "https://img.deezer.com/.../xl.jpg",
  "coverUrlSource": "deezer"
}
```

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
- `GET /version` → `{"version":"v0.2.1"}`; not rate limited.

## Database layout

`METADATA_DB_PATH` stores tracks, metadata, cover URLs, provenance, and metadata
FTS5. `LYRICS_DB_PATH` stores lyrics bodies and their track associations. The
`COVER_DB_PATH` stores album and artist cover URLs (and checked-misses) in a
separate database. The service composes responses in Go; it does not use
cross-database SQL joins. For an existing combined database, run the service
once with that file as the metadata path and a new lyrics path to copy lyrics and
remove the old combined lyrics tables safely.

## Album & artist cover behavior

Album and artist artwork is resolved by chaining three keyless sources in a
fixed fallback order: **Last.fm** (HTML scrape), then **iTunes** (Search API,
`entity=album`), then **Deezer** (`/search/artist` and `/search/album`). The
first non-empty URL wins. Artist art on iTunes/Deezer is the artwork of the
top-ranked album. The chain is throttled so iTunes is never hit more than once
every two seconds, and Last.fm is scraped conservatively (~1 request per 2s).
Spot URL upgrades rewrite low-resolution Last.fm CDN segments to `300x300` and
iTunes `100x100` artwork to `600x600`.

Both positive results and checked misses are upserted into `COVER_DB_PATH`; a
miss is served from cache (without spending upstream budget) until the
negative-cache window elapses. Only URLs are cached — cover image bytes are
never downloaded.

## Provider and cache behavior

iTunes and Deezer (secondary) are consulted on metadata misses in that order.
Identical metadata lookups share an in-process cache with bounded lifetimes,
including negative (not-found) results, so repeated misses and duplicate search
rows stop re-hitting upstream. Provider responses are bounded and cached
transactionally in SQLite. Search endpoints are local-only to keep upstream
traffic predictable.

LRCLIB remains the lyrics provider and uses its existing bounded client,
User-Agent, timeout, and cache behavior.

## Rate limiting

`RATE_LIMIT_PER_SEC` controls burst/token-bucket rate and
`RATE_LIMIT_PER_MIN` controls the rolling-minute cap. Rate-limited responses
include `Retry-After`.
