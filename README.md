# music-utils

`music-utils` is a small, single-binary Go server that provides a fast,
two-database SQLite music metadata and lyrics lookup API. It is designed for one
server process with minimal dependencies and a JSON-only HTTP interface.

## Roadmap

### Done

- **Lyrics database** — local exact lookup, FTS5 search, rate limiting, and
  optional LRCLIB fallback.
- **Song metadata database** — title, artist, album, duration, genre, year,
  release date, ISRC, MusicBrainz identifiers, and provenance are cached in
  SQLite.
- **Song cover URLs** — front-cover URLs are resolved through Cover Art Archive,
  cached with their source, and returned in metadata responses.
- **Searchable catalog** — local FTS5 search covers title, artist, album, and
  genre.

### Planned

- Album and artist cover URLs.
- Additional provider adapters and optional offline MusicBrainz dump import.
- Confidence scores and richer multi-value genre/tag storage.

## API

| Endpoint | Description |
| --- | --- |
| `GET /healthz` | Health check. Not rate limited. |
| `GET /version` | Running application version. Not rate limited. |
| `GET /api/metadata/get` | Exact song metadata lookup; local-first with MusicBrainz + Cover Art Archive fallback. |
| `GET /api/metadata/search` | Local metadata search; never calls upstream APIs. |
| `GET /api/cover/get` | Cached song cover URL and cover source. |
| `GET /api/lyrics/get` | Exact lyrics lookup; local-first with optional LRCLIB fallback. |
| `GET /api/lyrics/search` | Local lyrics/catalog search; never calls LRCLIB. |

The previous `/api/get` and `/api/search` paths are intentionally removed.
There are no aliases or compatibility redirects.

Examples:

```sh
curl http://localhost:8080/healthz
curl 'http://localhost:8080/api/metadata/get?track_name=Example%20Song&artist_name=Example%20Artist'
curl 'http://localhost:8080/api/metadata/search?q=example&limit=20'
curl 'http://localhost:8080/api/cover/get?track_name=Example%20Song&artist_name=Example%20Artist'
curl 'http://localhost:8080/api/lyrics/get?track_name=Example%20Song&artist_name=Example%20Artist'
curl 'http://localhost:8080/api/lyrics/search?q=example&limit=20'
```

Full request and response reference is in [`API.md`](API.md).

## Local-first caching

Metadata and lyrics are stored in independent SQLite files. Metadata and lyrics lookups check their respective local database before making an upstream request.
Successful provider responses are upserted transactionally and subsequent
requests are served locally. Identical concurrent MusicBrainz lookups share a
single in-flight request. The MusicBrainz and Cover Art Archive client uses
the configured `RATE_LIMIT_PER_SEC` burst and `RATE_LIMIT_PER_MIN` rolling
window, uses bounded response bodies, reuses HTTP connections, and sends a
descriptive User-Agent.

Metadata responses expose provenance:

- `metadataSource` — currently `musicbrainz` for enriched metadata.
- `coverUrlSource` — currently `cover_art_archive` for artwork.
- lyrics retain their existing `source` in the database.

The dedicated cover endpoint is cache-only: it never spends upstream budget.
Use `/api/metadata/get` when a cache miss should trigger enrichment.

## Provider research decision

The default provider pair is **MusicBrainz + Cover Art Archive**:

- no API key or end-user credentials;
- stable MusicBrainz IDs and rich recording/release/artist metadata;
- MusicBrainz core data is CC0 and the APIs are intended to be used politely
  with caching;
- Cover Art Archive provides front-cover metadata and resized image URLs.

MusicBrainz requires a descriptive User-Agent. This service applies the
configured `RATE_LIMIT_PER_SEC` and `RATE_LIMIT_PER_MIN` limits to outbound
metadata and artwork requests; operators should choose values compatible with
upstream provider policies. Commercial APIs such as Spotify, Deezer, Last.fm, and Discogs
can provide useful enrichment, but require credentials and have more
restrictive API terms/caching conditions, so they are not enabled by default.

## Features

- **FTS5 search** — title, artist, album, and genre search over SQLite.
- **Metadata fallback** — MusicBrainz lookup plus Cover Art Archive enrichment.
- **Lyrics fallback** — LRCLIB exact lookup and cache.
- **Rate limiting** — per-client-IP limits with `Retry-After` headers.
- **Structured logging** — request outcome labels such as `local_hit`,
  `musicbrainz_fallback_hit`, `lrclib_fallback_hit`, `miss`, and
  `rate_limited`.
- **Pure-Go build** — CGO-free SQLite via `modernc.org/sqlite`.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `METADATA_DB_PATH` | `./data/metadata.db` | SQLite database containing tracks, covers, provenance, and metadata FTS. |
| `LYRICS_DB_PATH` | `./data/lyrics.db` | SQLite database containing lyrics and lyrics associations. |
| `DB_MMAP_SIZE` | `536870912` | SQLite mmap size in bytes. |
| `DB_CACHE_SIZE_KB` | `-64000` | SQLite page cache size. |
| `DB_MAX_OPEN_CONNS` | `16` | SQLite connection pool limit. |
| `RATE_LIMIT_PER_SEC` | `10` | Per-IP token-bucket rate. |
| `RATE_LIMIT_PER_MIN` | `180` | Per-IP rolling-minute cap. |
| `TRUST_PROXY` | `false` | Trust the first `X-Forwarded-For` address. |
| `METADATA_FALLBACK_ENABLED` | `true` | Enable MusicBrainz/Cover Art Archive fallback. |
| `MUSICBRAINZ_BASE_URL` | `https://musicbrainz.org/ws/2` | MusicBrainz API base URL. |
| `COVER_ART_ARCHIVE_BASE_URL` | `https://coverartarchive.org` | Cover Art Archive base URL. |
| `MUSICBRAINZ_USER_AGENT` | `music-utils/v0.2.0 (+https://gru0.dev)` | Required descriptive upstream User-Agent. |
| `MUSICBRAINZ_TIMEOUT_MS` | `10000` | Metadata provider timeout. |
| `LRCLIB_FALLBACK_ENABLED` | `true` | Enable LRCLIB fallback. |
| `LRCLIB_BASE_URL` | `https://lrclib.net/api` | LRCLIB API base URL. |
| `LRCLIB_USER_AGENT` | `music-utils/v0.2.0 (+https://gru0.dev)` | LRCLIB User-Agent. |
| `LRCLIB_TIMEOUT_MS` | `5000` | LRCLIB timeout. |

## Database migration

New installations create `METADATA_DB_PATH` and `LYRICS_DB_PATH` independently.
On startup, an existing combined database is upgraded in place when it is used
as the metadata path: lyrics rows are copied to the lyrics database, metadata
tracks are rebuilt without a cross-database foreign key, and the old lyrics
 tables are removed only after the copy succeeds. Set the two paths explicitly
when migrating an existing `./data/music-utils.db` deployment.

## Quick start

```sh
go run ./cmd/server
```

Build a standalone binary with:

```sh
./build.sh
```

## Project layout

```
cmd/server/              main entry point
internal/config/         environment configuration and validation
internal/db/             SQLite connections, independent schemas, migration, and queries
internal/httpserver/     HTTP routes, handlers, middleware, rate limiting
internal/lrclib/         LRCLIB upstream client
internal/musicbrainz/    MusicBrainz + Cover Art Archive client
internal/version/        application version metadata
API.md                   complete HTTP API reference
```
