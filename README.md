# music-utils

`music-utils` is a small, single-binary Go server that provides a fast,
two-database SQLite music metadata and lyrics lookup API. It is designed for one
server process with minimal dependencies and a JSON-only HTTP interface.

## Roadmap

### Done

- **Lyrics database** — local exact lookup, FTS5 search, rate limiting, and
  optional LRCLIB fallback.
- **Song metadata database** — title, artist, album, duration, genre, year,
  release date, ISRC, and provenance are cached in SQLite.
- **Song cover URLs** — cover URLs are kept only when a metadata provider
  includes them for free; cached with their source, and returned in metadata responses.
- **Album and artist cover URLs** — a dedicated cover database and two
  enrichment endpoints resolve album/artist artwork from Last.fm, iTunes, and
  Deezer in order, caching URLs (and checked misses).
- **Searchable catalog** — local FTS5 search covers title, artist, album, and
  genre.

### Planned

- Confidence scores and richer multi-value genre/tag storage.

## API

| Endpoint | Description |
| --- | --- |
| `GET /healthz` | Health check|
| `GET /version` | Running application version.|
| `GET /api/metadata/get` | Exact song metadata lookup; local-first with iTunes + Deezer provider fallback. |
| `GET /api/metadata/search` | Local metadata search; never calls upstream APIs. |
| `GET /api/cover/get` | Cached song cover URL and cover source. |
| `GET /api/cover/artist` | Artist cover URL; resolves Last.fm → iTunes → Deezer on a miss and caches. |
| `GET /api/cover/album` | Album cover URL; resolves Last.fm → iTunes → Deezer on a miss and caches. |
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
curl 'http://localhost:8080/api/cover/artist?artist_name=Radiohead'
curl 'http://localhost:8080/api/cover/album?artist_name=Radiohead&album_name=OK%20Computer'
curl 'http://localhost:8080/api/lyrics/get?track_name=Example%20Song&artist_name=Example%20Artist'
curl 'http://localhost:8080/api/lyrics/search?q=example&limit=20'
```

Full request and response reference is in [`API.md`](API.md).

## Local-first caching

Metadata and lyrics are stored in independent SQLite files. Metadata and lyrics lookups check their respective local database before making an upstream request.
Successful provider responses are upserted transactionally and subsequent
requests are served locally. Metadata misses are resolved by a provider chain
that consults iTunes first, then Deezer, and an in-process cache memoizes both
hits and not-found misses with bounded lifetimes so repeated lookups stop
re-hitting upstream providers.

Metadata responses expose provenance:

- `metadataSource` — `itunes`, `deezer`, or user-provided.
- `coverUrlSource` — `itunes` or `deezer`, only when a provider returned a cover
  URL for free.
- lyrics retain their existing `source` in the database.

The dedicated cover endpoint is cache-only: it never spends upstream budget.
Use `/api/metadata/get` when a cache miss should trigger enrichment.

## Provider research decision

The default provider pair is **iTunes + Deezer**:

- no API key or end-user credentials;
- a single unauthenticated lookup returns track metadata plus a cover URL when
  available, replacing the previous multi-step MusicBrainz + Cover Art Archive
  chain;
- iTunes is fast and keyless (published guidance ~20 calls/min; be gentle);
- Deezer is a keyless secondary with strong ISRC and cover metadata (~50
  req/5s);
- a cover URL is only persisted when a provider includes one in its response.

Previously the service used MusicBrainz + Cover Art Archive with a dedicated
cover-resolution step. The Cover Art Archive call was the dominant source of
cold-lookup latency and has been removed.

## Features

- **FTS5 search** — title, artist, album, and genre search over SQLite.
- **Metadata fallback** — iTunes + Deezer provider chain with local caching.
- **Lyrics fallback** — LRCLIB exact lookup and cache.
- **Rate limiting** — per-client-IP limits with `Retry-After` headers.
- **Structured logging** — request outcome labels such as `local_hit`,
  `provider_fallback_hit`, `lrclib_fallback_hit`, `miss`, and
  `rate_limited`.
- **Pure-Go build** — CGO-free SQLite via `modernc.org/sqlite`.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `METADATA_DB_PATH` | `./data/metadata.db` | SQLite database containing tracks, covers, provenance, and metadata FTS. |
| `LYRICS_DB_PATH` | `./data/lyrics.db` | SQLite database containing lyrics and lyrics associations. |
| `COVER_DB_PATH` | `./data/cover.db` | SQLite database containing album/artist cover URLs and checked-misses. |
| `DB_MMAP_SIZE` | `536870912` | SQLite mmap size in bytes. |
| `DB_CACHE_SIZE_KB` | `-64000` | SQLite page cache size. |
| `DB_MAX_OPEN_CONNS` | `16` | SQLite connection pool limit. |
| `RATE_LIMIT_PER_SEC` | `10` | Per-IP token-bucket rate. |
| `RATE_LIMIT_PER_MIN` | `180` | Per-IP rolling-minute cap. |
| `TRUST_PROXY` | `false` | Trust the first `X-Forwarded-For` address. |
| `METADATA_FALLBACK_ENABLED` | `true` | Enable iTunes + Deezer metadata fallback. |
| `ITUNES_BASE_URL` | `https://itunes.apple.com` | iTunes Search API base URL. |
| `DEEZER_BASE_URL` | `https://api.deezer.com` | Deezer API base URL. |
| `METADATA_USER_AGENT` | `music-utils/v0.2.1 (+https://gru0.dev)` | Descriptive upstream User-Agent. |
| `METADATA_TIMEOUT_MS` | `5000` | Metadata provider timeout. |
| `COVER_FALLBACK_ENABLED` | `true` | Enable Last.fm + iTunes + Deezer album/artist cover resolution. |
| `COVER_TIMEOUT_MS` | `10000` | Album/artist cover provider timeout. |
| `COVER_USER_AGENT` | `music-utils/v0.2.1 (+https://gru0.dev)` | Cover upstream User-Agent. |
| `LASTFM_BASE_URL` | `https://www.last.fm` | Last.fm scraping base URL. |
| `LRCLIB_FALLBACK_ENABLED` | `true` | Enable LRCLIB fallback. |
| `LRCLIB_BASE_URL` | `https://lrclib.net/api` | LRCLIB API base URL. |
| `LRCLIB_USER_AGENT` | `music-utils/v0.2.1 (+https://gru0.dev)` | LRCLIB User-Agent. |
| `LRCLIB_TIMEOUT_MS` | `5000` | LRCLIB timeout. |

## Database migration

New installations create `METADATA_DB_PATH`, `LYRICS_DB_PATH`, and
`COVER_DB_PATH` independently.
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
internal/cover/          Last.fm + iTunes + Deezer album/artist cover providers and resolver
internal/db/             SQLite connections, independent schemas, migration, and queries
internal/httpserver/     HTTP routes, handlers, middleware, rate limiting
internal/lrclib/         LRCLIB upstream client
internal/metadata/       iTunes + Deezer metadata providers and resolver
internal/version/        application version metadata
API.md                   complete HTTP API reference
```
