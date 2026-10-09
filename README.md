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
- **Batch backfill jobs** — `metadata-backfill` and `lyrics-backfill`
  (`music-utils --run-job`) fill in metadata and lyrics for tracks that entered the
  library through a single lookup and were never resolved against a provider. Both
  take an optional `-refresh [age]`, share the live server's per-provider upstream
  budget, and only record a negative when every provider actually answered. See
  [Batch jobs](#batch-jobs).

### Planned

- Confidence scores and richer multi-value genre/tag storage.

## API

| Endpoint | Description |
| --- | --- |
| `GET /api/healthz` | Health check|
| `GET /api/version` | Running application version.|
| `GET /api/metadata/get` | Exact song metadata lookup; local-first with iTunes + Deezer provider fallback. |
| `GET /api/metadata/search` | Multi-provider metadata search across the local catalog, iTunes, and Deezer. |
| `GET /api/cover/get` | Song/album/artist cover URL; local-first, resolves iTunes/Deezer on a miss (songs and albums work without an artist). |
| `GET /api/cover/search` | Free-text cover search across artists, albums, and songs, plus typed per-type search. |
| `GET /api/cover/artist` | Artist cover URL; resolves Last.fm → iTunes → Deezer on a miss and caches. |
| `GET /api/cover/album` | Album cover URL; resolves Last.fm → iTunes → Deezer on a miss and caches. |
| `GET /api/lyrics/get` | Exact lyrics lookup; local-first with parallel multi-provider fallback (LRCLIB, BetterLyrics, KuGou, Paxsenix, LyricsPlus, YouTube via optional `video_id`). |
| `GET /api/lyrics/search` | Multi-result lyrics search across the local catalog and the same providers (videoId hint for YouTube). |

The previous `/api/get` and `/api/search` paths are intentionally removed.
There are no aliases or compatibility redirects.

Examples:

```sh
curl http://localhost:8080/api/healthz
curl 'http://localhost:8080/api/metadata/get?track_name=Example%20Song&artist_name=Example%20Artist'
curl 'http://localhost:8080/api/metadata/search?q=example&limit=20'
curl 'http://localhost:8080/api/cover/get?track_name=Example%20Song&artist_name=Example%20Artist'
curl 'http://localhost:8080/api/cover/search?q=hotel%20california&limit=10'
curl 'http://localhost:8080/api/cover/artist?artist_name=Radiohead'
curl 'http://localhost:8080/api/cover/album?artist_name=Radiohead&album_name=OK%20Computer'
curl 'http://localhost:8080/api/lyrics/get?track_name=Example%20Song&artist_name=Example%20Artist'
curl 'http://localhost:8080/api/lyrics/search?q=example&limit=20'
```

Full request and response reference is in [`API.md`](API.md).

## Local-first caching

Metadata and lyrics are stored in independent SQLite files. Metadata and lyrics lookups check their respective local database before making an upstream request. Lyrics misses fan out in parallel to every enabled provider — LRCLIB, BetterLyrics (TTML), KuGou, Paxsenix (Apple Music catalog + proxy), LyricsPlus (Binimum + mirror), plus YouTube (official shelf + transcript) when a `video_id` hint is supplied — and share the 3s response window. Direct Apple Music TTML and official Musixmatch also run in parallel when enabled; the former aggregation service is no longer used.

Before local or upstream lookup, music names are cleaned consistently across metadata, lyrics, and cover endpoints: known media extensions and downloader/source labels (for example `Official Music Video`, `AMV`, `Visualizer`, `Lyrics`, `Nightcore`, `Hardstyle`, `Sped Up`, and `Slowed`) are removed, and `Artist - Song`/`Artist ｜ Song` filenames can supply a missing artist. Explicit `artist_name` values remain authoritative, and provider-returned canonical names are preserved in responses.
Successful provider responses are upserted transactionally and subsequent
requests are served locally. Metadata misses are resolved by a provider chain
that consults iTunes first, then Deezer, and an in-process cache memoizes both
hits and not-found misses with bounded lifetimes so repeated lookups stop
re-hitting upstream providers.

Search is local-first with a background fill. `/api/metadata/search` ranks the
local catalog by relevance, deduplicates before applying `limit` so a page holds
`limit` distinct tracks, and prefers a fully described row over a bare stub of
the same song. If the strict match comes up short it retries locally against any
query word — which is what turns a typo or an abbreviation into a cache hit
instead of a provider round trip — and only then falls through to the provider
fan-out. The provider answer is persisted either way, so the next search for the
same query is a pure local read and the per-track `/api/metadata/get` for a
resolved track becomes a local hit too. When a search has nothing local to show
it waits on the provider fan-out for up to the 3s window, as lyrics does.
`/api/cover/search` caches whole responses per canonical query in the cover
database, including empty results, so a repeated query costs no upstream request
even after a restart. Both endpoints share one provider fan-out per query across
concurrent callers, so a burst of identical requests is asked once rather than
once per caller.

Metadata responses expose provenance:

- `metadataSource` — `itunes`, `deezer`, or user-provided.
- `coverUrlSource` — `itunes` or `deezer`, only when a provider returned a cover
  URL for free.
- lyrics retain their existing `source` in the database.

The song and album cover endpoints check local caches before resolving
upstream; songs and albums resolve on title/album alone when the artist is
omitted. Artist and album cover routes return the top result plus provider
results. Use `/api/cover/search` when you want an array of provider cover
results (free-text `q` searches artists, albums, and songs at once).

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
- **Lyrics providers** — LRCLIB, BetterLyrics, KuGou, Paxsenix, LyricsPlus, and YouTube (official + subtitle) plus optional direct Apple Music TTML and official Musixmatch, all fanned out in parallel and cached locally (videoId providers only when `video_id` is supplied; 3s response cap, background persistence).
- **Opt-in rich lyrics** — Unison-compatible word/syllable payloads are cached separately and returned alone with `include_rich_sync=true`; unavailable rich lyrics fall back to plain/LRC lyrics.
- **Rate limiting** — per-client-IP limits with `Retry-After` headers. A short-lived
  response replay cache sits in front of the limiter, so a request the server
  already answered is served from memory and consumes neither rate-limit budget
  nor provider budget. Rejections (`429`, `503`) are never cached, so a momentary
  upstream slowdown does not pin a URL to an error for the rest of the window.
- **Upstream pacing** — every provider (LRCLIB, BetterLyrics, KuGou, Paxsenix, LyricsPlus, YouTube, Apple Music, Musixmatch, iTunes, Deezer, Last.fm) is
  paced process-wide to a fixed interval, so no client traffic can exceed a
  provider's rate limit or get the server's IP blocked. The pace is also shared
  across processes through the database, so a batch job draws on the same budget
  instead of competing with live traffic for it — see
  [Sharing upstream budget with the server](#sharing-upstream-budget-with-the-server).
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
- **Background prefetch** — after a successful song lookup (metadata, lyrics,
  or song cover), the server quietly fetches and caches the song's lyrics,
  album cover, and artist cover (`PREFETCH_ENABLED`, default on) so later
  requests are local hits. Each target first checks the local caches and only
  spends upstream budget when something is genuinely missing; all calls flow
  through the same per-provider pacing as live traffic and a dedicated
  `PREFETCH_PER_MIN` budget (separate from client limits) caps background
  spend, so prefetching can never exhaust an upstream API.
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
| `RATE_LIMIT_PER_SEC` | `20` | Per-IP token-bucket rate. |
| `RATE_LIMIT_PER_MIN` | `600` | Per-IP rolling-minute cap. |
| `FALLBACK_PER_MIN` | `60` | Per-IP cap on cache-missing requests that trigger provider fallback. |
| `FALLBACK_MAX_QUEUE` | `50` | Max cache-missing requests inside the upstream layer; new misses fail fast with `503` when saturated. |
| `FALLBACK_QUEUE_WAIT_MS` | `10000` | How long a cache-missing request waits for an upstream queue slot before failing fast with `503`. |
| `TRUST_PROXY` | `false` | Trust the first `X-Forwarded-For` address. |
| `METADATA_FALLBACK_ENABLED` | `true` | Enable iTunes + Deezer metadata fallback. |
| `ITUNES_BASE_URL` | `https://itunes.apple.com` | iTunes Search API base URL. |
| `DEEZER_BASE_URL` | `https://api.deezer.com` | Deezer API base URL. |
| `METADATA_USER_AGENT` | `music-utils/v0.6.0 (+https://gru0.dev)` | Descriptive upstream User-Agent. |
| `METADATA_TIMEOUT_MS` | `5000` | Metadata provider timeout. |
| `COVER_FALLBACK_ENABLED` | `true` | Enable Last.fm + iTunes + Deezer album/artist cover resolution. |
| `COVER_TIMEOUT_MS` | `10000` | Album/artist cover provider timeout. |
| `COVER_USER_AGENT` | `music-utils/v0.6.0 (+https://gru0.dev)` | Cover upstream User-Agent. |
| `LASTFM_BASE_URL` | `https://www.last.fm` | Last.fm scraping base URL. |
| `COVER_REFRESH_ENABLED` | `true` | Background refresh of aged positive cover rows. |
| `COVER_REFRESH_AFTER_DAYS` | `30` | Revalidate cached positive cover URLs older than this. |
| `COVER_REFRESH_START_HOUR` | `2` | Refresh window start hour (server-local, 0–23). |
| `COVER_REFRESH_END_HOUR` | `5` | Refresh window end hour (exclusive; lower than start wraps across midnight). |
| `COVER_REFRESH_MAX_ROWS` | `2000` | Max cover rows checked per refresh sweep. |
| `COVER_REFRESH_MAX_RECHECK` | `200` | Max dead URLs re-resolved through providers per sweep. |
| `PREFETCH_ENABLED` | `true` | Background prefetch of related content after successful song lookups. |
| `PREFETCH_PER_MIN` | `10` | Cap on background upstream calls per minute (separate from client budgets). |
| `PREFETCH_CONCURRENCY` | `4` | Max prefetch jobs processed at once. |
| `PREFETCH_QUEUE_SIZE` | `64` | Pending prefetch queue; jobs beyond it are dropped. |
| `PREFETCH_LYRICS` | `true` | Prefetch LRCLIB lyrics for looked-up songs. |
| `PREFETCH_ALBUM_COVER` | `true` | Prefetch album artwork once the song's album is known. |
| `PREFETCH_ARTIST_COVER` | `true` | Prefetch artist artwork once the song's artist is known. |
| `REQUEST_LOG_ENABLED` | `true` | Record every request (when, endpoint, params, outcome, split cache/upstream timings) into the request log database. |
| `REQUEST_LOG_DB_PATH` | `./data/request_log.db` | Storage-optimized request log database. |
| `REQUEST_LOG_RETENTION_DAYS` | `0` | Prune request log rows older than this daily; `0` or `-1` keeps everything forever (default). |
| `REQUEST_LOG_UA_OPTIMIZE` | `true` | Collapse well-known client User-Agents (curl, wget, browsers, ...) to short tokens in the request log to save storage. |
| `REQUEST_LOG_UA_SAVE_UNKNOWN` | `true` | When UA optimization is on and a User-Agent is unrecognized, save the full string (`true`) or drop it as empty (`false`). |
| `REQUESTS_TODAY_ENABLED` | `false` | Serve `GET /api/stats/requests-today`, reporting requests logged in the last 24 hours (rolling window, seeded from the request log; its own polls excluded). |
| `STATS_ENDPOINTS` | *(empty)* | Serve the `GET /api/stats/*` cache-count endpoints: a comma-separated subset of `metadata`, `lyrics`, `covers`, `total`, `songs`, or `all` for every endpoint. Empty enables none. Stats requests are never written to the request log database. |
| `LRCLIB_FALLBACK_ENABLED` | `true` | Enable LRCLIB fallback. |
| `LRCLIB_BASE_URL` | `https://lrclib.net/api` | LRCLIB API base URL. |
| `LRCLIB_USER_AGENT` | `music-utils/v0.6.0 (+https://gru0.dev)` | LRCLIB User-Agent. |
| `LRCLIB_TIMEOUT_MS` | `5000` | LRCLIB timeout. |
| `RICH_LYRICS_ENABLED` | `true` | Enable opt-in word/syllable synchronized lyrics enrichment. |
| `RICH_LYRICS_BASE_URL` | `https://unison.boidu.dev` | Unison-compatible rich lyrics API base URL. |
| `RICH_LYRICS_USER_AGENT` | `music-utils/v0.6.0 (+https://gru0.dev)` | Rich lyrics provider User-Agent. |
| `RICH_LYRICS_TIMEOUT_MS` | `5000` | Rich lyrics provider timeout. |
| `APPLE_MUSIC_ENABLED` | `false` | Enable direct Apple Music catalog/TTML lookup. |
| `APPLE_MUSIC_CATALOG_BASE_URL` | `https://api.music.apple.com` | Apple Music catalog API or compliant proxy base URL. |
| `APPLE_MUSIC_LYRICS_BASE_URL` | `https://api.music.apple.com` | Apple Music lyrics API or compliant proxy base URL. |
| `APPLE_MUSIC_STOREFRONT` | `us` | Apple Music storefront used for catalog and lyrics requests. |
| `APPLE_MUSIC_MEDIA_USER_TOKENS` | *(empty)* | Comma-separated media-user tokens when the configured Apple Music endpoint requires them. |
| `APPLE_MUSIC_TIMEOUT_MS` | `10000` | Apple Music provider timeout. |
| `MUSIXMATCH_ENABLED` | `false` | Enable the official Musixmatch provider. |
| `MUSIXMATCH_BASE_URL` | `https://api.musixmatch.com` | Musixmatch API base URL. |
| `MUSIXMATCH_API_KEY` | *(empty)* | Required when Musixmatch is enabled; obtain and use it under the applicable Musixmatch plan and terms. |
| `MUSIXMATCH_TIMEOUT_MS` | `10000` | Musixmatch provider timeout. |
| `BETTERLYRICS_ENABLED` | `true` | BetterLyrics TTML provider (`lyrics-api.boidu.dev`). |
| `BETTERLYRICS_BASE_URL` | `https://lyrics-api.boidu.dev` | BetterLyrics base URL. |
| `BETTERLYRICS_USER_AGENT` | `music-utils/v0.14.0 (+https://gru0.dev)` | BetterLyrics User-Agent. |
| `BETTERLYRICS_TIMEOUT_MS` | `5000` | BetterLyrics timeout. |
| `KUGOU_ENABLED` | `true` | KuGou 3-step lyrics provider. |
| `KUGOU_SEARCH_BASE_URL` | `https://mobileservice.kugou.com` | KuGou search host. |
| `KUGOU_LYRICS_BASE_URL` | `https://lyrics.kugou.com` | KuGou lyrics host. |
| `KUGOU_USER_AGENT` | `music-utils/v0.14.0 (+https://gru0.dev)` | KuGou User-Agent. |
| `KUGOU_TIMEOUT_MS` | `10000` | KuGou timeout. |
| `PAXSENIX_ENABLED` | `true` | Paxsenix Apple Music catalog + proxy. |
| `PAXSENIX_PROXY_BASE_URL` | `https://lyrics.paxsenix.org` | Paxsenix proxy base URL. |
| `PAXSENIX_APPLE_BASE_URL` | `https://beta.music.apple.com` | Apple site used for bearer-token scraping. |
| `PAXSENIX_USER_AGENT` | `music-utils/v0.14.0 (+https://gru0.dev)` | Paxsenix User-Agent. |
| `PAXSENIX_TIMEOUT_MS` | `10000` | Paxsenix timeout. |
| `LYRICSPLUS_ENABLED` | `true` | LyricsPlus Binimum + mirror. |
| `LYRICSPLUS_API_BASE_URL` | `https://lyrics-api.binimum.org` | LyricsPlus Binimum index URL. |
| `LYRICSPLUS_MIRRORS` | *(empty)* | Comma-separated LyricsPlus mirror hosts for `/v2/lyrics/get`; empty uses defaults. |
| `LYRICSPLUS_USER_AGENT` | `music-utils/v0.14.0 (+https://gru0.dev)` | LyricsPlus User-Agent. |
| `LYRICSPLUS_TIMEOUT_MS` | `10000` | LyricsPlus timeout. |
| `YOUTUBE_LYRICS_ENABLED` | `true` | YouTube official lyrics shelf (InnerTube, needs `video_id`). |
| `YOUTUBE_SUBTITLE_ENABLED` | `true` | YouTube subtitle transcript (InnerTube, needs `video_id`). |
| `YOUTUBE_BASE_URL` | `https://music.youtube.com/youtubei/v1` | InnerTube base URL. |
| `YOUTUBE_API_KEY` | *(empty)* | InnerTube API key; empty uses the public web client key. |
| `YOUTUBE_USER_AGENT` | `music-utils/v0.14.0 (+https://gru0.dev)` | InnerTube User-Agent. |
| `YOUTUBE_TIMEOUT_MS` | `10000` | InnerTube timeout. |

## Database migration

New installations create `METADATA_DB_PATH`, `LYRICS_DB_PATH`, and
`COVER_DB_PATH` independently. The lyrics migration also creates the additive
`lyrics_sync_variants` table for cached word/syllable payloads; existing lyrics
rows are not rewritten or backfilled.
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

## Request & cache stats

`music-utils stats` (or `go run ./cmd/server stats`) reads the SQLite databases
and prints cached content inventory (unique individual songs, total cached items,
and breakdowns of song metadata, lyrics, and covers) alongside operational
request performance percentiles, activity windows (24h, 7d, 30d, all-time),
daily histograms, top endpoints, and client User-Agents in ASCII/Unicode boxes:

```sh
# Inspect the default databases (REQUEST_LOG_DB_PATH, METADATA_DB_PATH, LYRICS_DB_PATH, COVER_DB_PATH)
go run ./cmd/server stats

# Or point at custom database paths with custom daily window and top list size:
./bin/music-utils stats -db ./data/request_log.db -metadata ./data/metadata.db -lyrics ./data/lyrics.db -cover ./data/cover.db -days 30 -top 15
```

## Batch jobs

Content that entered the library through a lookup is only ever as complete as that
one lookup. Two batch jobs fill the gaps, and both are safe to run against the same
databases the live server is using.

```sh
# List the available jobs
./bin/music-utils --jobs

# Fill in metadata for tracks that were cached from lyrics only
./bin/music-utils --run-job metadata-backfill

# Fill in lyrics for tracks that have none
./bin/music-utils --run-job lyrics-backfill

# See what would change, without writing anything
./bin/music-utils --run-job lyrics-backfill -dry-run
```

Both jobs share these flags:

| Flag | Meaning |
| --- | --- |
| `-concurrency N` | songs worked on at once (default 4) |
| `-rate N` | songs started per minute (default 0, provider pacing only) |
| `-limit N` | cap the number of tracks processed |
| `-dry-run` | report what would change without writing |
| `-refresh` | also re-fetch work a provider already answered |
| `-user-idle-gap D` | how long live traffic must stay quiet before touching a shared upstream |
| `-max-write-errors N` | abort after this many consecutive failed write batches |

Ctrl-C stops between work units. Everything already resolved is still committed, and
whatever was left is picked up by the next run.

### Refreshing settled answers

By default a job only touches work **no provider has ever answered for**. Once a
track has an answer it is left alone, because re-asking costs upstream budget and
the answer is usually still good.

`-refresh` re-asks anyway, which matters for two things: a negative (a track with no
metadata or no lyrics that a provider may have added since), and a track whose
cached answer has simply gone stale.

```sh
# Re-fetch everything, never-answered work first
./bin/music-utils --run-job lyrics-backfill -refresh

# Re-fetch only answers older than three days
./bin/music-utils --run-job metadata-backfill -refresh 3d

# Weeks work too, and so do plain Go durations
./bin/music-utils --run-job metadata-backfill -refresh 1w
./bin/music-utils --run-job metadata-backfill -refresh 72h
```

A bare `-refresh` and `-refresh 0` mean the same thing, and a negative age is
rejected rather than reinterpreted.

The two phases are ordered, not merged: the never-answered set is drained
completely before the refresh set begins, so a limited run spends its budget on
tracks that have never been asked rather than re-confirming ones that already have
an answer. Within the refresh set, the oldest answer goes first, and a track settled
before settle times were recorded counts as the oldest of all.

Note that a track's answer is only re-fetched when every provider it is asked
actually answers. A rate limit, a refusal, or a network failure leaves the track
untouched and schedules it for the next run, rather than recording a permanent
"this has no lyrics" on the strength of a bad minute.

### Scope of the lyrics job

`lyrics-backfill` fetches **plain and synced** lyrics. It does not:

- fetch from **YouTube**, which is keyed by a video ID the batch path has no way to
  resolve;
- write **word-level sync** payloads (Unison, and the TTML paths of Apple Music and
  the other rich providers). The server compacts TTML into a canonical form before
  storing it, and a job writing the raw payload would create rows in a different
  shape than the one path that reads them. These are filled in by the live request
  path as songs are played.

### Every provider is asked, and every answer is kept

The job does not stop at the first provider that has lyrics. Every configured
provider is asked about every song, and every answer is stored.

That costs more upstream budget per song than a sequential walk would, and it is
deliberate: a run that stops at the first hit stores one answer and learns nothing
about the rest, so a provider outage is invisible until the stored answer has to be
replaced. Asking everyone means the alternatives are already on disk.

The best answer is chosen by what it contains, not by which provider replied first
— synced lyrics beat plain, and a tie goes to the provider with better coverage.
That is the same ranking the live request path applies, so the job and the server
agree about which lyrics are the good ones. **Only the winner is served**: a track's
`last_lyrics_id` points at one row, and the losing answers are stored alongside it
rather than instead of it, tagged with the provider they came from. They are not
returned by a lookup today, but they are there for a provider that starts returning
worse lyrics, or goes away entirely, without another full-library pass. The pointer
only ever moves to a strict improvement, so a run that draws a plainer answer than
one already stored leaves the better one serving.

Identical answers still collapse into a single row. The lyrics table is keyed by
content hash, so two providers that return the same text share one row and do not
grow the database.

### How the work is spread: songs across providers, not providers across songs

Each provider is limited to **1 request per 5 seconds**
(`UPSTREAM_LYRICS_JOB_PACE_MS`), and several songs are worked on at once
(`-concurrency`, default 4). A song is not finished until every provider has
answered it.

The order matters as much as the rate. Asking all six providers about one song at
once would put six requests into the air simultaneously, but they are already
limited to one per interval each, so the extra concurrency cannot become extra
throughput — it can only become a queue in front of each provider. So the run asks
**one provider at a time per song** and spreads the songs across providers instead:
each of the slots in flight starts on a different provider, so four songs make
progress on four distinct upstreams rather than queueing behind one.

That costs the same number of requests at the same rate and leaves the parallelism
somewhere it can actually be used. The ceiling is `-concurrency` songs in flight,
capped at the number of configured providers, since extra slots on one provider
would only queue.

Answers are written **as each provider gives them**, not held to the end, so a run
you interrupt keeps what it got. A song is marked resolved only once every provider
has answered it; a song with no lyrics anywhere is marked resolved too, because
"no provider has these" is an answer worth recording.

### Resuming a half-finished song

Each provider attempt is recorded per song, which is what makes an interrupted run
cheap to finish rather than expensive to redo. A song the last run left partway is
asked only about the providers it has not already answered — one request instead of
six.

`-refresh` is the exception: it is you asking again on purpose, so it ignores that
record entirely and re-asks every provider.

### Sharing upstream budget with the server

Both jobs share one per-provider upstream budget with the live server, stored in the
metadata database, and both yield to real traffic: a job only reaches an upstream
during a window in which no request is pending. If the server is in steady use the
job still makes progress — it defers for a bounded time and then takes its slot — so
a busy server slows a run down rather than stalling it.

The two sides are paced at **different rates on the same lease**, because they are
doing different jobs. A live request is one person waiting on one answer, so it is
served at the rate that provider's client normally uses (between 2 and 5 requests a
second depending on the provider). A batch run is working through a library and
nobody is waiting on it, so it is served at its own slower rate. They still take
turns: a live request is never delayed by a job, and a job is only admitted while
the server is idle.

The two jobs are held to different rates from each other as well, because they spend
their budget differently. The lyrics backfill asks every configured provider about
every song, so one song costs five or six requests and a library pass costs that
times every track in it; it is served at `UPSTREAM_LYRICS_JOB_PACE_MS` (default
5000, one request every five seconds). The metadata backfill asks iTunes and Deezer
about one track at a time, so it stays at `UPSTREAM_JOB_PACE_MS` (default 2000, one
request every two seconds). One rate for both would have to be either too slow for a
two-upstream lookup or too fast for a five-provider pass. Each job's rate goes on the
shared lease for the upstreams that job asks, so raising one does not touch the
other.

A job always claims its own job rate, so the settings cannot be confused for one
another: `UPSTREAM_PACE_MS` overrides the live request path for every upstream at
once and never reaches background work, and neither job rate ever speeds up the
request path. `UPSTREAM_PACE_MS` is unset by default, which leaves each provider on
its own rate — the rates are not interchangeable, so there is deliberately no default
for it. Set it when an upstream starts throttling and the request path needs slowing
globally; a value of zero or less is read as unset rather than as an absence of
pacing.

That means the aggregate load on an upstream is the ceiling of either side rather
than the sum of both, and `-user-idle-gap` applies to the lyrics job as it does to
metadata. A shared lease that has stopped working is logged, rather than the two
processes silently reverting to private pacing.

`JOB_IDLE_GAP_MS` sets how long the server must stay quiet before a job is admitted.

## Running a public instance

The server ships no authentication by design. For a public instance behind a
trusted proxy, relax the per-IP limits to comfortable UX levels (cache hits
are cheap; the fallback budget and queue guard below still bound upstream
spend), trust your reverse proxy, and consider limiting the lyrics fallback:

```sh
RATE_LIMIT_PER_SEC=20
RATE_LIMIT_PER_MIN=600
FALLBACK_PER_MIN=60
FALLBACK_MAX_QUEUE=50
FALLBACK_QUEUE_WAIT_MS=10000
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
internal/lrclib/         LRCLIB upstream client (5-strategy search + duration ranking)
internal/ttml/             Shared TTML → LRC parser (BetterLyrics/Paxsenix/LyricsPlus/Apple)
internal/betterlyrics/     BetterLyrics TTML client
internal/kugou/            KuGou 3-step lyrics client
internal/paxsenix/         Paxsenix Apple-catalog + proxy client
internal/lyricsplus/       LyricsPlus Binimum + mirrors client
internal/innertube/        InnerTube YouTube official + transcript client
internal/applemusic/      Apple Music catalog and TTML lyrics client
internal/musixmatch/      Official Musixmatch lyrics client
internal/metadata/       iTunes + Deezer metadata providers and resolver
internal/pacer/          shared upstream request pacing
internal/reqlog/         storage-optimized per-request access log database
internal/version/        application version metadata
API.md                   complete HTTP API reference
DEPLOYMENT.md            deployment guide (systemd, proxy, backups)
```
