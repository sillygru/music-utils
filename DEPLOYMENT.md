# Deployment guide

`music-utils` is a single static Go binary with no runtime dependencies and no
container needed. It stores everything in three SQLite files. This guide
covers a production install on Linux with systemd and a reverse proxy for
TLS. Docker is neither required nor recommended for this service.

The project's public instance runs at `https://api.music.gru0.dev/api/` (API
reference in [`API.md`](API.md)); this guide covers deploying your own.

## Contents

1. [Requirements](#requirements)
2. [Installation](#installation)
3. [Directory layout and databases](#directory-layout-and-databases)
4. [Configuration](#configuration)
5. [Running under systemd](#running-under-systemd)
6. [Reverse proxy and TLS](#reverse-proxy-and-tls)
7. [Verifying the install](#verifying-the-install)
8. [Backups](#backups)
9. [Upgrades](#upgrades)
10. [Running a public instance](#running-a-public-instance)
11. [Operational notes](#operational-notes)

## Requirements

- **Linux** (x86-64 or arm64). macOS works fine for development.
- **A few tens of MB of RAM.** The process is small; the SQLite page caches
  are bounded by `DB_CACHE_SIZE_KB` (default 64 MiB) and `DB_MMAP_SIZE`
  (default 512 MiB, which is virtual address space, not committed memory).
- **Disk:** the three databases are compact (metadata rows, lyrics text, and
  cover URL strings — no image bytes are ever stored). Budget ~1 GB of free
  disk; actual usage will be far less for typical catalogs.
- **Outbound HTTPS** to the upstream providers and artwork CDNs: `itunes.apple.com`,
  `api.deezer.com`, `www.last.fm`, `lrclib.net`, plus the artwork CDN hosts
  (mzstatic, dzcdn, fastly) used by the cover refresh job.
- **systemd** (any mainstream distribution). Not required to use the service,
  just for this guide's unit file.

The binary is pure Go with CGO disabled, so it runs on any Linux
distribution without any packages installed.

## Installation

### Option A — download a release binary (recommended)

Each release tags `internal/version/version.go` (e.g. `v0.3.0`) and attaches a
single `bin/music-utils` artifact to the GitHub release.

```sh
sudo install -d -o root -g root /opt/music-utils/bin
sudo install -m 0755 bin/music-utils /opt/music-utils/bin/music-utils
```

### Option B — build from source

Requires Go (any recent release). The build is CGO-free:

```sh
./build.sh          # produces ./bin/music-utils
sudo install -m 0755 bin/music-utils /opt/music-utils/bin/music-utils
```

### Create the service user and data directory

The service runs as its own unprivileged user:

```sh
sudo useradd --system --home /opt/music-utils --shell /usr/sbin/nologin music-utils
sudo install -d -o music-utils -g music-utils /opt/music-utils/data
```

## Directory layout and databases

```
/opt/music-utils/
├── bin/
│   └── music-utils          # the binary
├── data/
│   ├── metadata.db          # tracks, metadata, provenance, FTS5
│   ├── lyrics.db            # lyrics bodies and track associations
│   └── cover.db             # album/artist cover URLs and checked-misses
└── (music-utils.db-wal / -shm files appear next to each .db while running)
```

Database paths default to `./data/…` **relative to the working directory**,
so the systemd unit below pins `WorkingDirectory=/opt/music-utils`. You can
place the databases anywhere by setting `METADATA_DB_PATH`, `LYRICS_DB_PATH`,
and `COVER_DB_PATH` — they must be three **distinct** files, and each must be
writable by the service user. SQLite runs in WAL mode; the `-wal`/`-shm`
sidecar files live next to each database and must be backed up together with
it (see [Backups](#backups)).

The databases are caches, not an authoritative store. Deleting them is
recoverable: the server re-fetches from upstream on demand. Cover URLs can
rotate at their CDNs over time, which is exactly what the cover refresh job
exists to repair.

## Configuration

All configuration is via environment variables (full reference table in the
[README](README.md#configuration)). Create an env file for systemd:

```sh
sudo install -d -o root -g root /etc/music-utils
sudo touch /etc/music-utils/env
sudo chmod 600 /etc/music-utils/env
sudoedit /etc/music-utils/env
```

A minimal production file:

```sh
PORT=8080
LOG_LEVEL=info
METADATA_DB_PATH=/opt/music-utils/data/metadata.db
LYRICS_DB_PATH=/opt/music-utils/data/lyrics.db
COVER_DB_PATH=/opt/music-utils/data/cover.db
RATE_LIMIT_PER_SEC=10
RATE_LIMIT_PER_MIN=180
FALLBACK_PER_MIN=5
FALLBACK_MAX_QUEUE=3
TRUST_PROXY=true
```

Notes:

- `.env.example` in the repo is a complete reference. If you copy it, remove
  the leading `[TEMPLATE]` marker line — systemd's `EnvironmentFile` format
  only accepts `KEY=value` and comment lines.
- `PORT` is the only listen knob; the server binds **all interfaces**
  (`:PORT`). Do not expose it to the internet directly — see
  [Running a public instance](#running-a-public-instance).
- `TRUST_PROXY=true` makes rate limiting use the first `X-Forwarded-For`
  address. Enable it **only** behind a reverse proxy that overwrites that
  header; otherwise clients can spoof their IP.
- The cover refresh window (`COVER_REFRESH_START_HOUR`/`END_HOUR`) uses
  server-local time, so set the machine's timezone deliberately.

## Running under systemd

Create `/etc/systemd/system/music-utils.service`:

```ini
[Unit]
Description=music-utils API
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=music-utils
Group=music-utils
WorkingDirectory=/opt/music-utils
EnvironmentFile=/etc/music-utils/env
ExecStart=/opt/music-utils/bin/music-utils
Restart=on-failure
RestartSec=5

# The server shuts down gracefully on SIGTERM (5s drain timeout).
KillSignal=SIGTERM
TimeoutStopSec=15

# Hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/music-utils/data

[Install]
WantedBy=multi-user.target
```

Enable and start:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now music-utils
systemctl status music-utils
journalctl -u music-utils -f
```

Logs are structured JSON written to stderr, so journald captures them
directly; no log file configuration is needed. Startup logs print the active
configuration (paths, limits, provider URLs, cover refresh window) — check
them once after install to confirm the environment was picked up.

## Reverse proxy and TLS

The service has **no authentication** and binds all interfaces. Put it behind
a reverse proxy on the same host and terminate TLS there.

### Caddy (simplest)

```caddyfile
example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Caddy obtains certificates automatically and sets `X-Forwarded-For`, which is
why `TRUST_PROXY=true` is safe here.

### nginx

```nginx
server {
    listen 443 ssl;
    server_name example.com;
    ssl_certificate     /etc/letsencrypt/live/example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

For nginx, obtain certificates with certbot. The proxy must overwrite
`X-Forwarded-For` (as both examples above do) before you enable
`TRUST_PROXY=true`.

## Verifying the install

```sh
curl http://localhost:8080/healthz      # {"status":"ok"}  (not rate limited)
curl http://localhost:8080/version      # {"version":"v0.3.0"}
curl 'http://localhost:8080/api/metadata/get?track_name=Example%20Song&artist_name=Example%20Artist'
```

Through the proxy:

```sh
curl https://example.com/healthz
```

## Backups

All three databases can be backed up **online** (while the server runs) with
the SQLite CLI — the `.backup` command is safe under WAL mode:

```sh
mkdir -p /backups
sqlite3 /opt/music-utils/data/metadata.db ".backup '/backups/metadata-$(date +%F).db'"
sqlite3 /opt/music-utils/data/lyrics.db   ".backup '/backups/lyrics-$(date +%F).db'"
sqlite3 /opt/music-utils/data/cover.db    ".backup '/backups/cover-$(date +%F).db'"
```

Daily cron job (`crontab -e`):

```cron
0 3 * * *  /usr/bin/mkdir -p /backups && for db in metadata lyrics cover; do /usr/bin/sqlite3 /opt/music-utils/data/$db.db ".backup '/backups/$db-$(date +\%F).db'"; done; find /backups -name '*.db' -mtime +30 -delete
```

> If `sqlite3` is not installed, `music-utils export` (below) is a
> zero-dependency alternative for metadata and cover, and stopping the service
> briefly and copying the `.db` files (including any `-wal` file) also works.

**Restore:** stop the service, copy the backup back over the live file (or
point `METADATA_DB_PATH`/`LYRICS_DB_PATH`/`COVER_DB_PATH` at the backup), and
start it. Migrations run automatically on startup, so an older backup opened
by a newer binary is upgraded in place.

### Seeding a fresh instance

`music-utils export` produces compact, redistributable seed dumps of the
metadata and cover databases (`VACUUM INTO`):

```sh
sudo -u music-utils /opt/music-utils/bin/music-utils export \
  -metadata /opt/music-utils/data/metadata.db \
  -cover    /opt/music-utils/data/cover.db \
  -out      /backups/dump
```

This writes `metadata-dump.sqlite3` and `cover-dump.sqlite3`; point a new
instance's `METADATA_DB_PATH`/`COVER_DB_PATH` at them to start with a warm
cache. **Lyrics are intentionally excluded** — full lyrics are copyrighted
content owned by their rightsholders; point `LRCLIB_BASE_URL` at lrclib.net
(or a self-hosted LRCLIB) instead. Cover URLs can expire at their CDNs, so
treat a cover dump as a cache seed, not a permanent store.

## Upgrades

Releases are version-tagged (e.g. `v0.3.0`) and attach the binary to the
GitHub release. Upgrading:

1. Back up the databases (see [Backups](#backups)) — always before an upgrade.
2. Download the new `bin/music-utils` and install it over the old one.
3. `sudo systemctl restart music-utils`.
4. Check `journalctl -u music-utils -e` for the startup log and migration
   messages, then confirm `/healthz`.

Schema migrations run automatically at startup and are idempotent. Keep the
previous binary (e.g. `music-utils.old`) for an instant rollback:
`systemctl stop music-utils`, swap the binaries, `systemctl start`.

## Running a public instance

The server ships **no authentication by design**. For a public, rate-limited
instance:

1. **Do not expose `:8080`.** Restrict the firewall to the proxy ports and
   deny everything else (the server cannot be told to bind localhost):

   ```sh
   sudo ufw default deny incoming
   sudo ufw allow 80/tcp      # redirect to https
   sudo ufw allow 443/tcp
   sudo ufw enable
   ```

2. **Tighten the per-IP limits** in `/etc/music-utils/env` (see the README's
   "Running a public instance" section for the rationale):

   ```sh
   RATE_LIMIT_PER_SEC=2
   RATE_LIMIT_PER_MIN=60
   FALLBACK_PER_MIN=5
   FALLBACK_MAX_QUEUE=3
   TRUST_PROXY=true
   # optionally: LRCLIB_FALLBACK_ENABLED=false (serve only cached lyrics)
   ```

3. **Protection that does not depend on IPs:** per-provider pacing is
   process-wide, LRCLIB misses are negative-cached for 24h, the per-IP
   fallback budget caps upstream spend per client, and the queue gate fails
   fast with `503` when the upstream layer is saturated. Together these keep
   upstream spend bounded no matter how many clients call.

4. **Monitor:** `/healthz` is unauthenticated and unthrottled — point your
   uptime checker at it. Watch journald for `rate_limited` and `503`
   outcomes; those counters are the signal for tuning the limits.

## Operational notes

- **One process is the design.** `music-utils` is built for a single server
  process holding the three databases. Run one instance; don't shard.
- **Cover refresh timing.** The hourly refresh sweep only runs inside
  `COVER_REFRESH_START_HOUR`–`END_HOUR` in server-local time and draws from
  the same upstream pacing as live traffic, so it can never exceed provider
  rate limits. Pick a window that is quiet for your users.
- **Time.** Rate limiting uses wall-clock seconds; the refresh window uses
  local time. NTP matters for `Retry-After` accuracy.
- **Disk growth is bounded.** Rows are upserted by key (name/artist/album/
  duration for tracks, name/artist for covers) — repeated lookups do not
  accumulate duplicates. The only unbounded-in-memory structures are bounded:
  the lyrics negative cache (100k entries) and the provider memo cache
  (bounded lifetimes).
- **Logging.** JSON to stderr; `LOG_LEVEL=debug` traces upstream provider
  calls if you need to observe pacing.
