# music-utils API

The `music-utils` server exposes a small, JSON-only HTTP API. All endpoints
below are served on the configured port (`:8080` by default).

## Common conventions

- All responses are `application/json`.
- Errors use `{"code": <http_status>, "message": "<description>"}`.
- `/healthz` is never rate limited; `/api/*` requests are rate limited per
  client IP (see [Rate limiting](#rate-limiting)).
- Successful `GET /api/get` responses return a single **Track** object.
  Successful `GET /api/search` responses return an array of **Track** objects.

### Track object

| Field | Type | Description |
| --- | --- | --- |
| `id` | integer | Database ID of the track. |
| `trackName` | string | Track title. |
| `artistName` | string | Artist name. |
| `albumName` | string | Album name (may be empty). |
| `duration` | number | Track duration in seconds (may be `0`). |
| `instrumental` | boolean | `true` if the track has no lyrics. |
| `plainLyrics` | string | Plain-text lyrics (may be empty). |
| `syncedLyrics` | string | Timestamped lyrics, LRC format (may be empty). |

```json
{
  "id": 1,
  "trackName": "Example Song",
  "artistName": "Example Artist",
  "albumName": "Example Album",
  "duration": 203.5,
  "instrumental": false,
  "plainLyrics": "Example lyrics...",
  "syncedLyrics": "[00:12.00] Example lyrics..."
}
```

## `GET /healthz`

Health check. Returns `{"status": "ok"}` with HTTP `200`. Not rate limited.

## `GET /version`

Returns the running application version. The current default is `v0.1.0`.
Returns HTTP `200` and is not rate limited.

```json
{"version":"v0.1.0"}
```

## `GET /api/get`

Exact lyrics lookup. Checks the local SQLite database first; when configured
with the LRCLIB fallback, a local miss is fetched from `lrclib.net` and cached
for future requests.

### Query parameters

| Parameter | Required | Description |
| --- | --- | --- |
| `track_name` | yes | Track title. |
| `artist_name` | yes | Artist name. |
| `album_name` | no | Album name; narrows the exact match when provided. |
| `duration` | no | Track duration in seconds (non-negative number); matches exactly when provided. |

### Responses

| Status | Body | Notes |
| --- | --- | --- |
| `200` | Track object | Local hit or LRCLIB fallback hit. |
| `400` | Error | Missing `track_name`/`artist_name` or invalid `duration`. |
| `404` | Error | Not found locally, and no LRCLIB fallback hit. |
| `429` | Error | Rate limited (includes `Retry-After` header). |
| `500` | Error | Internal error. |

### Example

```sh
curl 'http://localhost:8080/api/get?track_name=Example%20Song&artist_name=Example%20Artist&album_name=Example%20Album&duration=203.5'
```

## `GET /api/search`

Free-text search over the local lyrics database. Never calls LRCLIB.

### Query parameters

| Parameter | Required | Description |
| --- | --- | --- |
| `q` | no | Free-text query (searches track, artist, and album names). |
| `track_name` | no | Alternative to `q`; combined with the other fields when given. |
| `artist_name` | no | Alternative to `q`; combined with the other fields when given. |
| `album_name` | no | Alternative to `q`; combined with the other fields when given. |
| `limit` | no | Maximum results. Default `20`, range `1–50`. |

At least one of `q` or `track_name`/`artist_name`/`album_name` is required.

### Responses

| Status | Body | Notes |
| --- | --- | --- |
| `200` | Array of Track objects | May be an empty array. |
| `400` | Error | No query terms, or invalid `limit`. |
| `429` | Error | Rate limited (includes `Retry-After` header). |
| `500` | Error | Internal error. |

### Example

```sh
curl 'http://localhost:8080/api/search?q=example&limit=20'
```

## Rate limiting

`/api/*` requests are rate limited per client IP:

- `RATE_LIMIT_PER_SEC` (default `10`) — token-bucket burst limit.
- `RATE_LIMIT_PER_MIN` (default `180`) — rolling one-minute cap.

When a limit is exceeded the server responds `429` with a `Retry-After`
header. When running behind a trusted proxy, set `TRUST_PROXY=true` so the
real client IP is read from `X-Forwarded-For`.

## LRCLIB fallback

`GET /api/get` serves local data first. When there is no local match and
`LRCLIB_FALLBACK_ENABLED=true`, the server makes one bounded request to
`LRCLIB_BASE_URL`, sends a descriptive user agent, and caches successful
results locally (`source=lrclib_fallback`) so later lookups are served from
SQLite. A remote miss, timeout, or upstream failure results in a normal `404`.

## Errors

All error bodies follow `{"code": <http_status>, "message": "<description>"}`:

| Status | Message |
| --- | --- |
| `400` | `track_name is required`, `artist_name is required`, `duration must be a non-negative number`, `q or track_name, artist_name, or album_name is required`, `limit must be an integer between 1 and 50` |
| `404` | `Track not found` |
| `429` | `Rate limit exceeded` |
| `500` | `Internal server error` |
