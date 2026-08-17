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
  everyone.- **IDs:** `id` values are local row identifiers. They are stable within an instance but are not guaranteed to match across instances.
- **Name cleanup:** exact and search lookups remove common media-library filename extensions and source/style labels such as `Official Music Video`, `AMV`, `Visualizer`, `Lyrics`, `Nightcore`, `Hardstyle`, `Sped Up`, and `Slowed`. When `artist_name` is omitted, common `Artist - Song` and `Artist ｜ Song` filenames are split automatically. An explicit artist is authoritative; provider-canonical names are returned in successful responses.

## Endpoints

| Endpoint | Description |
| --- | --- |
| `GET /api/healthz` | Health check (not rate limited) |
| `GET /api/version` | Server version (not rate limited) |
| `GET /api/metadata/get` | Exact metadata lookup; resolves upstream on a miss |
| `GET /api/metadata/search` | Multi-provider metadata search; returns several results |
| `GET /api/metadata/get` | Top metadata result; returns one object |
| `GET /api/cover/search` | Free-text or typed cover search across artists, albums, and songs |
| `GET /api/cover/get` | Top cover result; returns one object |
| `GET /api/cover/artist` | Artist cover top result, with provider results included |
| `GET /api/cover/album` | Album cover top result, with provider results included |
| `GET /api/lyrics/get` | Exact/top lyrics lookup; returns one object |
| `GET /api/lyrics/search` | LRCLIB-compatible multi-result lyrics search |
| `GET /api/stats/requests-today` | Requests served in the last 24 hours (rolling window, opt-in) |
| `GET /api/stats/metadata` | Songs with cached metadata (opt-in) |
| `GET /api/stats/lyrics` | Songs with cached lyrics (opt-in) |
| `GET /api/stats/covers` | Cached cover entries, with song/album/artist breakdown (opt-in) |
| `GET /api/stats/total` | Everything cached summed: metadata + lyrics + covers (opt-in) |
| `GET /api/stats/songs` | Distinct individual track names cached (opt-in) |

## `GET /api/healthz` and `GET /api/version`

- `GET /api/healthz` → `{"status":"ok"}` — liveness probe; never rate limited.
- `GET /api/version` → `{"version":"v0.6.0"}` — never rate limited.

## `GET /api/stats/requests-today`

How many requests the instance has served in the last 24 hours. It is a
rolling window: requests drop off continuously as they pass 24 hours old,
rather than the count resetting at a fixed time of day. The count is seeded
from the request log at startup and updates live, so it survives restarts.
Its own requests are excluded, so polling it does not inflate the number.

Example response:

```json
{
  "requestsToday": 4123
}
```

This endpoint is **opt-in** (`REQUESTS_TODAY_ENABLED=true`) and defaults to
off; when disabled it returns `404`. It depends on the request log
(`REQUEST_LOG_ENABLED`) for a real count and reports `0` when logging is
turned off. Subject to the same per-IP rate limits as the rest of `/api/*`.
Like every `/api/stats/*` endpoint, its requests are never written to the
request log database.

## `GET /api/stats/metadata`, `/api/stats/lyrics`, `/api/stats/covers`, `/api/stats/total`, `/api/stats/songs`

How many songs are cached, split by cache. Each endpoint is individually
**opt-in** through the `STATS_ENDPOINTS` environment variable
(`metadata,lyrics,covers,total,songs`, or `all` for every endpoint) and
defaults to off; when an endpoint is not enabled it returns `404`. These are
pure local cache counts — no upstream calls ever happen. Their requests are
never written to the request log database, and they are subject to the same
per-IP rate limits as the rest of `/api/*`.

Example responses:

```json
{ "metadataSongs": 51234 }

{ "lyricsSongs": 48177 }

{
  "covers": 6611,
  "songCovers": 4420,
  "albumCovers": 1174,
  "artistCovers": 1017
}

{
  "total": 106022,
  "metadata": 51234,
  "lyrics": 48177,
  "covers": 6611
}

{ "songs": 48453 }
```

Semantics:

- `metadataSongs` — rows in the metadata cache (`tracks`): every cached song,
  including songs whose cache entry only carries lyrics or a cover.
- `lyricsSongs` — songs with a cached lyrics association. Sharing one lyrics
  row between songs still counts each song once.
- `covers` = `songCovers` + `albumCovers` + `artistCovers`. `songCovers` counts
  songs whose metadata cache carries a cover URL; `albumCovers`/`artistCovers`
  count positive album and artist cover entries. Checked-miss rows (no art
  found) are never counted.
- `total` = `metadata` + `lyrics` + `covers`, the unified amount of everything
  cached (a song in several caches counts once per cache).
- `songs` — distinct individual track names cached (case-insensitive), so a
  song cached in several caches is still counted once.

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
or `type=song`. A song request without `type` defaults to `song` for
compatibility.The endpoint checks local caches before resolving the provider chain. Album
and artist responses include a `results` array with every cached provider URL;
song responses keep the single-object shape.

Query parameters: `type` is optional and defaults to `song`; `track_name` is
required for songs and `album_name` for albums. `artist_name` is optional for
songs and albums — when omitted, the title/album is searched on its own
(iTunes and Deezer resolve name-only lookups; Last.fm still needs an artist).
`artist_name` is required for `type=artist`.

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

Two search modes, both returning a JSON array of cover results.

**Free-text search (default when `q` is set):** searches songs, albums, and
artists for one query and merges the results, interleaved so no type crowds
out the others. Each result carries an `entityType` (`song`, `album`, or
`artist`) plus the usual `trackName`/`artistName`/`albumName`,
`coverUrl`, and `coverUrlSource` fields. Songs come from iTunes and Deezer's
free-text search; albums and artists from Last.fm, iTunes, and Deezer. Add
`type=artist`, `type=album`, or `type=song` to narrow the free-text search to
one kind.

**Structured search (when `q` is omitted):** `type` is required and selects
which artwork to look up:

```sh
# Free-text, mixed: songs + albums + artists
curl 'https://music.gru0.dev/api/cover/search?q=hotel+california&limit=10'
# Free-text narrowed to one kind
curl 'https://music.gru0.dev/api/cover/search?q=radiohead&type=artist'
# Structured per-type search
curl 'https://music.gru0.dev/api/cover/search?type=artist&artist_name=Radiohead'
curl 'https://music.gru0.dev/api/cover/search?type=album&album_name=OK%20Computer'
curl 'https://music.gru0.dev/api/cover/search?type=song&track_name=No%20Surprises'
```

`artist_name` is optional in structured song/album searches, same as
`/api/cover/get`. `limit` defaults to `10`, range `1–50`; it caps the final
merged array.`## `GET /api/cover/artist`

Returns artist artwork as a cover URL. On a miss it resolves the provider chain
(Last.fm → iTunes → Deezer) in order, caches **every plausible provider URL**
(not just the winner) in the cover database, and returns the winner as
`coverUrl`. If the winner later dies, a still-live cached alternate is served
without contacting any provider.

Query parameter: required `artist_name`.

Example response:

```json
{
  "id": 1,
  "entityType": "artist",
  "artistName": "Radiohead",
  "coverUrl": "https://is1-ssl.mzstatic.com/…/600x600bb.jpg",
  "coverUrlSource": "itunes",
  "results": [
    {
      "entityType": "artist",
      "artistName": "Radiohead",
      "coverUrl": "https://is1-ssl.mzstatic.com/…/600x600bb.jpg",
      "coverUrlSource": "itunes"
    }
  ]
}
```

Responses: `200` JSON array (possibly empty) · `400` invalid input · `429` rate
limited · `503` upstream busy · `500` internal error.

The artist and album endpoints also expose the selected top-level cover for
backward compatibility and include a `results` array containing every cached
provider URL — the full list is served from the store on cache hits, not just
on the first lookup. Use `/api/cover/search` when you only want the array.

## `GET /api/cover/album`

Album artwork, with the same enrichment, variant caching, and `results`
behavior as `/api/cover/artist`: every plausible provider URL is cached and the
winner is returned as `coverUrl`.

Query parameters: required `album_name`; `artist_name` is optional. When the
artist is omitted the album is searched on its own as a best-effort lookup
(Last.fm still needs an artist, so name-only results come from iTunes and
Deezer); the first exact-title match wins.

Example response:

```json
{
  "id": 1,
  "entityType": "album",
  "artistName": "Radiohead",
  "albumName": "OK Computer",
  "coverUrl": "https://e-cdns-images.dzcdn.net/…/xl.jpg",
  "coverUrlSource": "deezer",
  "results": [
    {
      "entityType": "album",
      "artistName": "Radiohead",
      "albumName": "OK Computer",
      "coverUrl": "https://e-cdns-images.dzcdn.net/…/xl.jpg",
      "coverUrlSource": "deezer"
    }
  ]
}
```

Responses: `200` cover URL · `400` invalid input · `404` provider miss
(memoized for 24 hours) · `429` rate limited · `503` upstream busy · `500`
internal error.

## `GET /api/lyrics/get`

Exact lyrics lookup. Serves cached lyrics when available; on a miss it
consults LRCLIB and caches the result.

Query parameters: required `track_name` and `artist_name`; optional
`album_name`, non-negative `duration`, `include_rich_sync=true`, and optional
`sync_type=word|syllable|richsync`. Without `include_rich_sync=true`, the
response contains the available plain and/or line-synchronized lyrics. With
`include_rich_sync=true`, the server first tries an additional
Unison-compatible lookup; a successful rich lookup returns only `richSync`,
while a rich miss falls back to the plain/LRC lyrics fields.

Example response:

```json
{
  "id": 42,
  "name": "No Surprises",
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
| `name` | string | Display name; always mirrors `trackName`. |
| `trackName` | string | Song title. |
| `artistName` | string | Artist name. |
| `albumName` | string | Album/release title. |
| `duration` | number | Seconds. |
| `instrumental` | boolean | True for instrumental tracks (lyrics fields empty). |
| `plainLyrics` | string | Plain-text lyrics. |
| `syncedLyrics` | string | Timestamped LRC lyrics, when available. |
| `richSync` | object | Source-native word/syllable synchronized payload; returned alone when `include_rich_sync=true` and a provider has a result. If unavailable, the response falls back to `plainLyrics` and/or `syncedLyrics`. |
| `richSync.content` | object | Compact parsed rich-sync JSON with `title`, `artist`, `duration`, and `lines`. Each line is `[begin, end, text, words]`; each word is `[begin, end, text]`. |
| `richSync.content.title` | string | Song title, when present. |
| `richSync.content.artist` | string | Artist name, when present. |
| `richSync.content.duration` | number | Song duration in seconds. |
| `richSync.content.lines` | array | Timed line tuples; word timestamps are nested in the fourth item. |
| `richSync.format` | string | `json` for the compact server representation. |
| `richSync.syncType` | string | Synchronization level such as `word`, `syllable`, or `richsync`. |
| `richSync.source` | string | Provider name, currently `unison`. |

A rich response has this compact content shape:

```json
{
  "title": "Somebody's Pleasure",
  "artist": "Aziz Hedra",
  "duration": 223.98,
  "lines": [[7.184, 13.436, "I've been so busy, ignoring, and hiding", [[7.184, 7.532, "I've"], [7.532, 7.819, "been"]]]]
}
```

The response intentionally does not include LRCLIB's redundant `lyricsfile`
YAML field. Rich payloads are cached separately in the lyrics database as
compact JSON, and existing TTML rows are converted asynchronously after
startup. When a rich payload is returned, it replaces the plain and
line-synchronized fields; when it is unavailable, the endpoint falls back to
the cached or LRCLIB plain/LRC fields.

Responses: `200` lyrics object · `400` invalid/missing input · `404` not
found (memoized for 24 hours) · `429` rate limited · `503` upstream busy ·
`500` internal error.

## `GET /api/lyrics/search`

Searches the local catalog and merges it with LRCLIB's `/api/search`
results. Each result carries the compact fields `id`, `name`, `trackName`,
`artistName`, `albumName`, `duration`, `instrumental`, `plainLyrics`, and
`syncedLyrics` when available. The redundant LRCLIB `lyricsfile` YAML field is
not returned.

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

- **20 requests/second** and **600 requests/minute** on all `/api/*` endpoints.
- **Cache hits do not consume the stricter upstream budget below**, but every
  `/api/*` request counts toward the per-IP limits above.
- Only requests that miss the cache and actually fetch from an upstream
  source count against a separate, stricter cap: **60 upstream-triggering
  requests/minute**. A client that repeatedly queries content the API does
  not have will hit this second cap and receive `429`.
- When the shared upstream queue is saturated, new misses wait for a slot
  (up to `FALLBACK_QUEUE_WAIT_MS`) and then fail fast with `503` instead of
  queueing indefinitely — retry after the `Retry-After` interval.

`/api/healthz` and `/api/version` are never rate limited. Limit values are current
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
- **Album and artist covers keep every provider URL.** Lookups cache all
  plausible results (Last.fm, iTunes, Deezer), serve the best as `coverUrl`,
  and return the full list in `results` — on cache hits too. If the winner
  URL dies, a live cached alternate is promoted without an upstream call.

## Self-hosting

The server is open source (MIT) and ships as a single static binary. A
self-hosted instance serves the exact same endpoints at the same paths —
clients only need a different base URL. See the project repository's
README and deployment guide for configuration and operations.
