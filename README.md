# music-utils

`music-utils` is a small, single-binary Go server that provides a fast,
SQLite-backed lyrics lookup API modeled after LRCLIB. It is designed for one
server process with minimal dependencies and a JSON-only HTTP interface.

## Roadmap

### Done

- **Lyrics database** — a local SQLite-backed lyrics database with exact
  lookup, full-text search, rate limiting, and optional LRCLIB fallback.

### Planned

The next milestone turns `music-utils` from a lyrics-only store into a richer
music metadata service:

- **Song metadata database** — extend the current `tracks` table into a fuller
  song metadata catalog (beyond name, artist, album, and duration), keeping
  the existing FTS5 search and exact-lookup behavior.
- **Song cover URLs** — store a cover-art URL per track and expose it through
  the API responses.
- **Album and artist cover URLs** — store cover-art URLs at the album and
  artist level and surface them alongside song results.

## API

The server exposes a small JSON-only HTTP interface with four endpoints.
Full reference — parameters, response shapes, errors, and examples — is in
[`API.md`](API.md).

| Endpoint | Description |
| --- | --- |
| `GET /healthz` | Health check. Not rate limited. |
| `GET /version` | Running application version. Not rate limited. |
| `GET /api/get` | Exact lyrics lookup by track and artist. |
| `GET /api/search` | Free-text search over the local lyrics database. |

Basic usage:

```sh
# Health check
curl http://localhost:8080/healthz

# Application version
curl http://localhost:8080/version

# Exact lookup
curl 'http://localhost:8080/api/get?track_name=Example%20Song&artist_name=Example%20Artist&album_name=Example%20Album&duration=203.5'

# Free-text search
curl 'http://localhost:8080/api/search?q=example&limit=20'
```

API errors use `{"code": ..., "message": ...}`. `/api/*` requests are rate
limited per client IP; see [`API.md`](API.md) for details.

---

The rest of this document covers running, configuring, and deploying the
server.

## Versioning

The application currently reports version `v0.1.0` from `GET /version`. The
same version is included in startup logs and the default LRCLIB user-agent.
`internal/version/version.go` is the single source of truth; update its
`Version` value (for example, to `v0.2.0`) and merge the change to `main`.
The GitHub Actions release workflow then builds the binary, creates the matching
`v...` tag, and publishes a GitHub release whose title and description come
from the triggering commit.

## Quick start

```sh
go run ./cmd/server
```

The server listens on `:8080` by default and logs one structured JSON line per
request at `info` level. Override settings with environment variables or a
shell environment file:

```sh
PORT=9090 LOG_LEVEL=debug RATE_LIMIT_PER_SEC=10 RATE_LIMIT_PER_MIN=180 go run ./cmd/server
```

## Features

- **Free-text search** — SQLite FTS5 full-text search across track, artist,
  and album names.
- **LRCLIB fallback** — transparently fetch and cache missing tracks from
  `lrclib.net` (opt-in via config).
- **Rate limiting** — per-client-IP limits with `Retry-After` headers.
- **Structured logging** — one JSON log line per request, with outcome labels
  (`local_hit`, `lrclib_fallback_hit`, `miss`, `rate_limited`, …) for
  operational diagnosis.
- **Pure-Go build** — CGO-free SQLite via `modernc.org/sqlite`; deploys as a
  single static binary.

## LRCLIB fallback

`/api/get` checks SQLite first. When there is no local match and
`LRCLIB_FALLBACK_ENABLED=true`, the server makes one bounded request to
`LRCLIB_BASE_URL` (`/get`), sends the configured descriptive user agent, and
caches successful lyrics locally with `source=lrclib_fallback`. Later exact
lookups are served from SQLite. A remote miss, timeout, or other upstream
failure remains a normal local `404` and is visible as a warning in logs for
operational diagnosis. `/api/search` never calls LRCLIB.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port (1–65535). |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `DB_PATH` | `./data/music-utils.db` | SQLite database path. |
| `DB_MMAP_SIZE` | `536870912` | SQLite mmap size in bytes. |
| `DB_CACHE_SIZE_KB` | `-64000` | SQLite page cache size; negative values are KiB. |
| `DB_MAX_OPEN_CONNS` | `16` | SQLite connection pool limit. |
| `RATE_LIMIT_PER_SEC` | `10` | Per-IP token-bucket rate. |
| `RATE_LIMIT_PER_MIN` | `180` | Per-IP rolling-minute cap. |
| `TRUST_PROXY` | `false` | Trust the first `X-Forwarded-For` address. |
| `LRCLIB_FALLBACK_ENABLED` | `true` | Enable request-triggered exact-lookup fallback. |
| `LRCLIB_BASE_URL` | `https://lrclib.net/api` | LRCLIB API base URL; useful for tests. |
| `LRCLIB_USER_AGENT` | `music-utils/v0.1.0 (+https://gru0.dev)` | Descriptive upstream user agent. |
| `LRCLIB_TIMEOUT_MS` | `5000` | Upstream request timeout in milliseconds. |

Invalid explicitly supplied settings fail startup with a clear error.

## Build and deploy

The SQLite driver is pure Go. Build a standalone binary without CGO:

```sh
./build.sh
```

Copy `bin/music-utils` and your environment configuration to the target host,
then run:

```sh
./bin/music-utils
```

No Docker or container runtime is required.

## Project layout

```
cmd/server/            main entry point (config load, DB open/migrate, HTTP server)
internal/config/       environment-based configuration with validation
internal/db/           SQLite connection, embedded schema, migrations, queries
internal/httpserver/   HTTP routes, handlers, middleware, rate limiting
internal/lrclib/       LRCLIB upstream client (exact lookup)
internal/version/      application version metadata
API.md                 complete HTTP API reference
```
