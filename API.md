# music-utils API

A public, no-authentication API for music metadata, lyrics, and cover-art URLs.

**Base URL:** `https://music.gru0.dev/api/`

No API key, no registration, no authentication. All responses are JSON.
Self-hosted instances serve the identical endpoints at the same paths — see
[Self-hosting](#self-hosting) at the bottom.

## Quick start

```sh
curl 'https://music.gru0.dev/api/metadata/get?track_name=Paranoid%20Android&artist_name=Radiohead'
```

```js
const res = await fetch(
  'https://music.gru0.dev/api/lyrics/get?track_name=No%20Surprises&artist_name=Radiohead'
);
const data = await res.json();
console.log(data.plainLyrics);
```

## Conventions

- **Errors** are always `{"code": <http_status>, "message": "<description>"}`.
- **Status codes:** `200` ok · `400` invalid input · `404` not found ·
  `429` rate limited · `503` upstream temporarily busy · `500` internal error.
- **Rate limits** apply per client IP (see [Rate limits](#rate-limits)).
  `429` and `503` responses carry a `Retry-After` header — honor it.
- **CORS:** the API is callable directly from browsers
  (`Access-Control-Allow-Origin: *`).
- **Etiquette:** send a descriptive `User-Agent` with contact information
  (e.g. `my-app/1.0 (+https://example.com)`). The API proxies keyless upstream
  sources with strict rate caps, so heavy or abusive usage degrades it for
  everyone.
- **IDs:** `id` values are local row identifiers. They are stable within an
  instance but are not guaranteed to match across instances.

## Endpoints

| Endpoint | Description |
| --- | --- |
| `GET /healthz` | Health check (not rate limited) |
| `GET /version` | Server version (not rate limited) |
| `GET /api/metadata/get` | Exact metadata lookup; resolves upstream on a miss |
| `GET /api/metadata/search` | Multi-provider metadata search; returns several results |
| `GET /api/metadata/get` | Top metadata result; returns one object |
| `GET /api/cover/search` | Multi-provider artist, album, or song cover search |
| `GET /api/cover/get` | Top cover result; returns one object |
| `GET /api/cover/artist` | Artist cover top result, with provider results included |
| `GET /api/cover/album` | Album cover top result, with provider results included |
| `GET /api/lyrics/get` | Exact/top lyrics lookup; returns one object |
| `GET /api/lyrics/search` | LRCLIB-compatible multi-result lyrics search |

## `GET /healthz` and `GET /version`

- `GET /healthz` → `{"status":"ok"}` — liveness probe; never rate limited.
- `GET /version` → `{"version":"v0.5.1"}` — never rate limited.

## `GET /api/metadata/get`

Exact song lookup by title and artist. Serves the cached record when
available; on a miss it resolves through the upstream providers (iTunes, then
Deezer), stores the result, and returns it. A cover URL is included when a
provider returns one.

Query parameters:

- `track_name` — required.
- `artist_name` — required.
- `album_name` — optional narrowing hint.
- `duration` — optional non-negative seconds hint.

Example response:

```json
{
  "id": 42,
  "trackName": "Paranoid Android",
  "artistName": "Radiohead",
  "albumName": "OK Computer",
  "duration": 383,
  "genre": "Alternative",
  "year": 1997,
  "releaseDate": "1997-05-28T07:00:00Z",
  "isrc": "GBSTW9700021",
  "musicbrainzRecordingId": "…",
  "musicbrainzReleaseId": "…",
  "musicbrainzReleaseGroupId": "…",
  "musicbrainzArtistId": "…",
  "coverUrl": "https://is1-ssl.mzstatic.com/…/600x600bb.jpg",
  "metadataSource": "itunes",
  "coverUrlSource": "itunes"
}
```

| Field | Type | Description |
| --- | --- | --- |
| `id` | integer | Local track ID. |
| `trackName` | string | Song title. |
| `artistName` | string | Artist name. |
| `albumName` | string | Album/release title. |
| `duration` | number | Seconds. |
| `genre` | string | Best available genre/tag (omitted when unknown). |
| `year` | integer | Release year (omitted when unknown). |
| `releaseDate` | string | Provider release date (omitted when unknown). |
| `isrc` | string | ISRC, when available. |
| `musicbrainzRecordingId` | string | Canonical recording MBID, when available. |
| `musicbrainzReleaseId` | string | Selected release MBID, when available. |
| `musicbrainzReleaseGroupId` | string | Release-group MBID, when available. |
| `musicbrainzArtistId` | string | Primary artist MBID, when available. |
| `coverUrl` | string | Cover URL returned by a provider, when available. |
| `metadataSource` | string | Metadata provenance: `itunes`, `deezer`, or user-provided. |
| `coverUrlSource` | string | Artwork provenance: `itunes` or `deezer`, when a cover URL is present. |

Responses: `200` metadata object · `400` invalid/missing input · `404` not
found · `429` rate limited · `503` upstream busy · `500` internal error.

## `GET /api/metadata/search`

Searches the local catalog and merges matching results from iTunes and
Deezer. Results are deduplicated by track, artist, and album; each result
retains its provider provenance. The final response is a JSON array.

Query parameters:

- `q`, or one or more of `track_name`, `artist_name`, `album_name`, `genre`.
- `limit`, default `20`, range `1–50`.

```sh
curl 'https://music.gru0.dev/api/metadata/search?q=radiohead&limit=20'
```

Responses: `200` JSON array of metadata objects (possibly empty) · `400`
invalid input · `429` rate limited · `500` internal error.

## `GET /api/cover/get`

Returns the top cover result as one object. Use `type=artist`, `type=album`,
or `type=song` with `artist_name` and the corresponding optional/required
name fields. A song request without `type` remains supported for compatibility.
The endpoint checks local caches before resolving the provider chain.

Query parameters: `type` is optional and defaults to `song`; `artist_name` is
required; `track_name` is required for songs; `album_name` is required for
albums.

Example response:

```json
{
  "id": 42,
  "trackName": "Paranoid Android",
  "artistName": "Radiohead",
  "albumName": "OK Computer",
  "coverUrl": "https://is1-ssl.mzstatic.com/…/600x600bb.jpg",
  "coverUrlSource": "itunes"
}
```

Responses: `200` cached cover · `400` invalid input · `404` not cached ·
`429` rate limited · `500` internal error.

## `GET /api/cover/search`

Searches artwork across Last.fm, iTunes, and Deezer. The response is an array
with one entry per provider that returned a URL; `coverUrlSource` identifies the
provider. Set `type=artist`, `type=album`, or `type=song`.

```sh
curl 'https://music.gru0.dev/api/cover/search?type=artist&artist_name=Radiohead'
curl 'https://music.gru0.dev/api/cover/search?type=album&artist_name=Radiohead&album_name=OK%20Computer'
curl 'https://music.gru0.dev/api/cover/search?type=song&artist_name=Radiohead&track_name=No%20Surprises'
```

## `GET /api/cover/artist`

Returns artist artwork as a cover URL. On a miss it resolves the provider
chain (Last.fm → iTunes → Deezer) in order, caches the winning URL, and
returns it.

Query parameter: required `artist_name`.

Example response:

```json
{
  "id": 1,
  "entityType": "artist",
  "artistName": "Radiohead",
  "coverUrl": "https://is1-ssl.mzstatic.com/…/600x600bb.jpg",
  "coverUrlSource": "itunes"
}
```

Responses: `200` JSON array (possibly empty) · `400` invalid input · `429` rate
limited · `503` upstream busy · `500` internal error.

The artist and album endpoints also expose the selected top-level cover for
backward compatibility and include a `results` array containing the provider
results. Use `/api/cover/search` when you only want the array.

## `GET /api/cover/album`

Album artwork, with the same enrichment and caching behavior as
`/api/cover/artist`.

Query parameters: required `artist_name` and `album_name`.

Example response:

```json
{
  "id": 1,
  "entityType": "album",
  "artistName": "Radiohead",
  "albumName": "OK Computer",
  "coverUrl": "https://e-cdns-images.dzcdn.net/…/xl.jpg",
  "coverUrlSource": "deezer"
}
```

Responses: `200` cover URL · `400` invalid input · `404` provider miss
(memoized for 24 hours) · `429` rate limited · `503` upstream busy · `500`
internal error.

## `GET /api/lyrics/get`

Exact lyrics lookup. Serves cached lyrics when available; on a miss it
consults LRCLIB and caches the result.

Query parameters: required `track_name` and `artist_name`; optional
`album_name` and non-negative `duration`.

Example response:

```json
{
  "id": 42,
  "trackName": "No Surprises",
  "artistName": "Radiohead",
  "albumName": "OK Computer",
  "duration": 229,
  "instrumental": false,
  "plainLyrics": "A heart that's full up like a landfill…",
  "syncedLyrics": "[00:00.00] A heart that's full up like a landfill…"
}
```

| Field | Type | Description |
| --- | --- | --- |
| `id` | integer | Local track ID. |
| `trackName` | string | Song title. |
| `artistName` | string | Artist name. |
| `albumName` | string | Album/release title. |
| `duration` | number | Seconds. |
| `instrumental` | boolean | True for instrumental tracks (lyrics fields empty). |
| `plainLyrics` | string | Plain-text lyrics. |
| `syncedLyrics` | string | Timestamped LRC lyrics, when available. |

Responses: `200` lyrics object · `400` invalid/missing input · `404` not
found (memoized for 24 hours) · `429` rate limited · `503` upstream busy ·
`500` internal error.

## `GET /api/lyrics/search`

Searches the local catalog and merges it with LRCLIB's `/api/search`
results. LRCLIB returns a JSON array containing fields such as `id`,
`trackName`, `artistName`, `albumName`, `duration`, `instrumental`,
`plainLyrics`, and `syncedLyrics`.

Query parameters: `q`, or one or more of `track_name`, `artist_name`,
`album_name`; optional `limit` from `1–50`, default `20`. `q` is passed to
LRCLIB's `/api/search`; the server applies the final limit after merging.

```sh
curl 'https://music.gru0.dev/api/lyrics/search?q=no%20surprises&limit=20'
```

Responses: `200` JSON array of lyrics objects (possibly empty) · `400`
invalid input · `429` rate limited · `500` internal error.

## Rate limits

Current public policy, applied per client IP:

- **2 requests/second** and **60 requests/minute** on all `/api/*` endpoints.
- **Cache hits do not consume the stricter upstream budget below**, but every
  `/api/*` request counts toward the per-IP limits above.
- Only requests that miss the cache and actually fetch from an upstream
  source count against a separate, stricter cap: **5 upstream-triggering
  requests/minute**. A client that repeatedly queries content the API does
  not have will hit this second cap and receive `429`.
- When the shared upstream queue is saturated, new misses fail fast with
  `503` instead of waiting — retry after the `Retry-After` interval.

`/healthz` and `/version` are never rate limited. Limit values are current
policy and may be adjusted; always honor `Retry-After` rather than assuming
fixed numbers.

## Caching and data notes

- **Request logging is server-side operational data.** Instances may record
  every request (timestamp, endpoint, params, status, cache/upstream outcome,
  and split timings) into a dedicated, storage-optimized local database. This
  data is never served by the API and is never included in seed dumps —
  clients' query parameters stay on the instance's own disk, subject to its
  retention policy (see the deployment guide).
- The catalog grows as content is requested; popular content is served from
  cache, while obscure content may take a moment on its first request.
- **Not-found results are memoized for 24 hours** (lyrics in memory, cover
  misses in the store). Re-requesting a missing song within the window will
  not trigger another upstream lookup — retrying in a tight loop is both
  pointless and rate-limited.
- **Cover URLs are CDN links, not image data.** They can rotate or expire at
  the CDN over time. If a client renders a broken image, re-request the
  endpoint — stale URLs are re-resolved automatically. The API never serves
  or stores image bytes.

## Self-hosting

The server is open source (MIT) and ships as a single static binary. A
self-hosted instance serves the exact same endpoints at the same paths —
clients only need a different base URL. See the project repository's
README and deployment guide for configuration and operations.
