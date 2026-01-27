# Inazuma

Inazuma is a front-end cache server for 52Poké Wiki. It sits between Nginx and MediaWiki, serves cached HTML from S3-compatible object storage, and refreshes cache entries on demand or via PURGE events.

## Features

- Variant-aware page cache (`zh`, `zh-hans`, `zh-hant`) based on `Accept-Language` or URL prefix
- Cache stampede protection via Redis locks
- S3-compatible object storage backend (Hetzner, MinIO, etc.)
- PURGE endpoint to refresh cache and purge Nginx cache

## Build

```
CGO_ENABLED=0 go build ./cmd/inazuma
```

## Run

```
INAZUMA_LISTEN_ADDR=:8080 \
INAZUMA_MEDIAWIKI_BASE_URL=https://wiki.example.com \
INAZUMA_REDIS_ADDR=redis:6379 \
INAZUMA_S3_ENDPOINT=https://s3.example.com \
INAZUMA_S3_REGION=eu-central-1 \
INAZUMA_S3_BUCKET=52poke-cache \
INAZUMA_S3_ACCESS_KEY=... \
INAZUMA_S3_SECRET_KEY=... \
INAZUMA_NGINX_PURGE_URL=http://nginx-52w \
./inazuma
```

## Cache rules (summary)

- Only `GET` requests are cacheable.
- `/wiki/Title` uses `Accept-Language` to select variant.
- `/zh/Title`, `/zh-hans/Title`, `/zh-hant/Title` force variants.
- `/index.php?title=Title` is cacheable; any extra query params (besides `utm_*`) are not cacheable.
- `Special:` pages are not cacheable.
- Non-200 responses are not cached.
- Expired cache entries are refreshed with a global lock; if unavailable, stale content is served and refreshed later.

## PURGE

Inazuma accepts HTTP `PURGE` and refreshes all variants for a title.

Required header:

```
X-Purge-Timestamp: 2026-01-27T12:34:56Z
```

Title sources (first match wins):
- `title` query param
- `X-Title` header
- JSON body `{ "title": "..." }`

If the cache entry has `updated_at` later than the timestamp, the refresh is skipped.
Non-200 (non-5xx) refresh results delete the cached object to avoid stale entries.

## Docker

```
docker build -t ghcr.io/OWNER/inazuma:local .
```

## Environment variables

- `INAZUMA_LISTEN_ADDR` (default `:8080`)
- `INAZUMA_MEDIAWIKI_BASE_URL` (required)
- `INAZUMA_REDIS_ADDR` (required)
- `INAZUMA_REDIS_DB` (default `0`)
- `INAZUMA_REDIS_PASSWORD`
- `INAZUMA_S3_ENDPOINT` (required)
- `INAZUMA_S3_REGION` (required)
- `INAZUMA_S3_BUCKET` (required)
- `INAZUMA_S3_ACCESS_KEY` (required)
- `INAZUMA_S3_SECRET_KEY` (required)
- `INAZUMA_NGINX_PURGE_URL` (optional; empty disables nginx purge)
- `INAZUMA_LOGGED_IN_COOKIE` (default `52poke_wikiUserID`)
- `INAZUMA_CACHE_TTL_SECONDS` (default `2592000` / 30 days)
- `INAZUMA_LOCK_TTL_SECONDS` (default `45`)
- `INAZUMA_MAX_LOCK_WAIT_SECONDS` (default `3`)
