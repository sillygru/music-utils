# music-utils

`music-utils` is a small, single-binary Go server that provides a fast,
three-database SQLite music metadata, lyrics, and cover-art URL lookup API. It
is designed for one server process with minimal dependencies and a JSON-only
HTTP interface.

A public instance of this API is hosted at
**`https://music.gru0.dev/api/`** — consumer documentation is in
[`API.md`](API.md). The rest of this README covers configuration and
self-hosting; the deployment guide is in [`DEPLOYMENT.md`](DEPLOYMENT.md).

## License

The software is released under the [MIT License](LICENSE). The license covers
the code only: cached lyrics are copyrighted content owned by their respective
rightsholders and are never redistributed by this project. Metadata and cover
URL dumps (see [Seed dumps](#seed-dumps)) contain only factual data and links.

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
| `GET /api/metadata/search` | Multi-provider metadata search across the local catalog, iTunes, and Deezer. |
| `GET /api/cover/get` | Cached song cover URL and cover source. |
| `GET /api/cover/artist` | Artist cover URL; resolves Last.fm → iTunes → Deezer on a miss and caches. |
| `GET /api/cover/album` | Album cover URL; resolves Last.fm → iTunes → Deezer on a miss and caches. |
| `GET /api/lyrics/get` | Exact lyrics lookup; local-first with optional LRCLIB fallback. |
| `GET /api/lyrics/search` | Multi-result lyrics search across the local catalog and LRCLIB. |

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

The song cover endpoint checks the metadata cache before resolving upstream;
artist and album cover routes return the top result plus provider results. Use
`/api/cover/search` when you want an array of provider cover results.

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
- **Upstream pacing** — every provider (LRCLIB, iTunes, Deezer, Last.fm) is
  paced process-wide to a fixed interval, so no client traffic can exceed a
  provider's rate limit or get the server's IP blocked.
- **Lyrics negative caching** — LRCLIB misses are memoized in memory for 24
  hours, so repeated lookups of non-existent songs never re-hit LRCLIB.
- **Fallback budget and queue guard** — a per-IP cap on upstream-triggering
  misses (`FALLBACK_PER_MIN`) plus a shared queue gate (`FALLBACK_MAX_QUEUE`)
  stop a single client from monopolizing the provider queue with garbage
  lookups; saturated queues fail fast with `503` instead of queueing everyone.
- **Cover URL self-healing** — cached positive cover rows older than
  `COVER_REFRESH_AFTER_DAYS` are revalidated by a background job inside the
  configured low-activity window (cheap range GET against the artwork CDN;
  dead URLs are re-resolved through the provider chain, capped per run). Stale
  positives are also re-resolved on demand when requested, so cover URL rot
  never leaves a permanent dead link.
- **Request logging** — every request is logged (when, endpoint, params,
  status, cached-or-upstream outcome, and split cache/upstream timings) into a
  dedicated, storage-optimized SQLite database (`REQUEST_LOG_ENABLED`, default
  on): integer timestamps, dictionary tables for repeated values, no secondary
  indexes, WAL + incremental auto-vacuum, batched writes, and daily retention
  pruning. The log is never served by the API and is never included in
  `music-utils export` dumps.
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
| `FALLBACK_PER_MIN` | `10` | Per-IP cap on cache-missing requests that trigger provider fallback. |
| `FALLBACK_MAX_QUEUE` | `5` | Max cache-missing requests inside the upstream layer; new misses fail fast with `503` when saturated. |
| `TRUST_PROXY` | `false` | Trust the first `X-Forwarded-For` address. |
| `METADATA_FALLBACK_ENABLED` | `true` | Enable iTunes + Deezer metadata fallback. |
| `ITUNES_BASE_URL` | `https://itunes.apple.com` | iTunes Search API base URL. |
| `DEEZER_BASE_URL` | `https://api.deezer.com` | Deezer API base URL. |
| `METADATA_USER_AGENT` | `music-utils/v0.5.1 (+https://gru0.dev)` | Descriptive upstream User-Agent. |
| `METADATA_TIMEOUT_MS` | `5000` | Metadata provider timeout. |
| `COVER_FALLBACK_ENABLED` | `true` | Enable Last.fm + iTunes + Deezer album/artist cover resolution. |
| `COVER_TIMEOUT_MS` | `10000` | Album/artist cover provider timeout. |
| `COVER_USER_AGENT` | `music-utils/v0.5.1 (+https://gru0.dev)` | Cover upstream User-Agent. |
| `LASTFM_BASE_URL` | `https://www.last.fm` | Last.fm scraping base URL. |
| `COVER_REFRESH_ENABLED` | `true` | Background refresh of aged positive cover rows. |
| `COVER_REFRESH_AFTER_DAYS` | `30` | Revalidate cached positive cover URLs older than this. |
| `COVER_REFRESH_START_HOUR` | `2` | Refresh window start hour (server-local, 0–23). |
| `COVER_REFRESH_END_HOUR` | `5` | Refresh window end hour (exclusive; lower than start wraps across midnight). |
| `COVER_REFRESH_MAX_ROWS` | `2000` | Max cover rows checked per refresh sweep. |
| `COVER_REFRESH_MAX_RECHECK` | `200` | Max dead URLs re-resolved through providers per sweep. |
| `REQUEST_LOG_ENABLED` | `true` | Record every request (when, endpoint, params, outcome, split cache/upstream timings) into the request log database. |
| `REQUEST_LOG_DB_PATH` | `./data/request_log.db` | Storage-optimized request log database. |
| `REQUEST_LOG_RETENTION_DAYS` | `30` | Prune request log rows older than this daily; `0` keeps everything. |
| `LRCLIB_FALLBACK_ENABLED` | `true` | Enable LRCLIB fallback. |
| `LRCLIB_BASE_URL` | `https://lrclib.net/api` | LRCLIB API base URL. |
| `LRCLIB_USER_AGENT` | `music-utils/v0.5.1 (+https://gru0.dev)` | LRCLIB User-Agent. |
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

## Seed dumps

`music-utils export` produces redistributable seed dumps of the metadata and
cover databases using SQLite's `VACUUM INTO`, so a fresh instance starts with a
warm cache instead of cold upstream lookups:

```sh
./bin/music-utils export -metadata ./data/metadata.db -cover ./data/cover.db -out ./dump
```

The flags default to `METADATA_DB_PATH` and `COVER_DB_PATH`. The dumps are plain
SQLite files: point `METADATA_DB_PATH`/`COVER_DB_PATH` at them to seed a new
instance. Lyrics are intentionally excluded from dumps — full lyrics are
copyrighted content owned by others and are available directly from LRCLIB, so
self-hosters should point `LRCLIB_BASE_URL` at lrclib.net (or a self-hosted
LRCLIB instance) rather than at a lyrics dump. The request log database is
operational data (timestamps, client params, latency) and is likewise never
exported. Cover URLs can rotate at their CDNs, so treat a cover dump as a
cache seed, not a permanent store.

## Running a public instance

The server ships no authentication by design. For a rate-limited public
instance, tighten the per-IP limits, trust your reverse proxy, and consider
limiting the lyrics fallback:

```sh
RATE_LIMIT_PER_SEC=2
RATE_LIMIT_PER_MIN=60
FALLBACK_PER_MIN=5
FALLBACK_MAX_QUEUE=3
TRUST_PROXY=true
# optionally: LRCLIB_FALLBACK_ENABLED=false (serve only cached lyrics)
```

`TRUST_PROXY=true` must only be set when the server is behind a trusted reverse
proxy that overwrites `X-Forwarded-For`; otherwise clients can spoof their IP.
Per-provider pacing, lyrics negative caching, the fallback budget, and the
queue guard keep upstream spend bounded regardless of how many client IPs are
calling, so the per-IP numbers above are a UX dial rather than the protection
itself.

## Project layout

```
cmd/server/              main entry point (server + export subcommand)
internal/config/         environment configuration and validation
internal/cover/          Last.fm + iTunes + Deezer album/artist cover providers and resolver
internal/db/             SQLite connections, independent schemas, migration, and queries
internal/httpserver/     HTTP routes, handlers, middleware, rate limiting, cover refresh job
internal/lrclib/         LRCLIB upstream client
internal/metadata/       iTunes + Deezer metadata providers and resolver
internal/pacer/          shared upstream request pacing
internal/reqlog/         storage-optimized per-request access log database
internal/version/        application version metadata
API.md                   complete HTTP API reference
DEPLOYMENT.md            deployment guide (systemd, proxy, backups)
```
