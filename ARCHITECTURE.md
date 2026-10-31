# picapi v2 — Architecture

Status: draft, under discussion. Nothing here is final until it is implemented.

## Goals

- Upload an image (later: any document) and get a short, permanent URL for it.
- Transform images on the fly with URL parameters: resize, crop, format, quality, background removal.
- Keep originals untouched; serve small, optimized variants to minimize egress and page weight.
- Self-hosted and open source: each user or project deploys their own single-tenant instance on their own domain, the way they would deploy Postgres.
- First-class clients: web UI, public HTTP API, CLI, and MCP.

## Non-goals (for now)

- Replacing an asset's bytes under the same ID. Assets are immutable: to change a picture, upload a new one and delete the old one. This keeps every URL cacheable forever and removes versioning from storage, database and API.
- Multi-tenancy, user accounts, per-user permissions.
- Transforms for video and PDF beyond generating a poster/thumbnail image (planned, see [Later](#later)).
- Migrating data from picapi v1.

## Overview

```
                       ┌──────────────────────────── Railway ───────────────────────────┐
browser / <img> ──▶ Cloudflare CDN ──miss──▶ picapi (Go) ──▶ S3-compatible bucket (private)
                    (edge cache)            │  ├─ libvips (govips)                      │
CLI / MCP / UI ─────────────────────────────▶  ├─ ONNX Runtime (background removal)     │
                                            │  └─ SQLite (metadata, on a volume)        │
                       └────────────────────────────────────────────────────────────────┘
```

- **picapi** is a single Go service. It runs the HTTP API and the MCP endpoint, and it serves the embedded web UI.
- **Object storage** is any S3-compatible bucket and is always private. The reference deployment uses a Railway Bucket.
- **Cloudflare** sits in front of the public hostname as a reverse proxy/CDN, with the DNS record proxied (orange cloud). It is the cache users actually hit.
- **SQLite** holds metadata and is the source of truth for which assets exist. Postgres is an optional alternative.

## Tech stack

| Concern | Choice | Notes |
|---|---|---|
| Language | Go | Single static binary for the CLI. Good concurrency and low memory for the server. |
| HTTP | Echo v5 | Already in use. |
| Image processing | libvips via `govips` | Fast and streaming, with JPEG/PNG/WebP/AVIF/HEIC/GIF support. Requires cgo. |
| Background removal | ONNX Runtime via `yalue/onnxruntime_go` | Same models rembg uses. Pre- and post-processing are ported to Go. |
| Metadata | SQLite (`modernc.org/sqlite`) | Optional `DATABASE_URL` for Postgres. |
| Object storage | `aws-sdk-go-v2` S3 client | Path-style addressing by default. Checksums are only sent when required (R2 compatibility). |
| MCP | Official MCP Go SDK | Served by the API at `/mcp`. |
| Web UI | Vanilla JS, embedded with `embed.FS` | No separate frontend deployment. |
| Packaging | Docker image | Contains libvips, ONNX Runtime and the default model. A `-slim` tag without ML is possible later. |

### Background-removal models

Only permissively licensed models are shipped or downloaded by default:

| `BG_MODEL` | License | Size | Notes |
|---|---|---|---|
| `u2netp` | Apache-2.0 | ~4 MB | Fast; for small instances. |
| `isnet` (default) | Apache-2.0 | ~170 MB | Good general quality. |
| `birefnet` | MIT | ~900 MB | Best quality; needs more RAM and CPU. |
| `off` | — | — | Disables `bg=remove`. |

BRIA RMBG models are **not** supported by default because their license is non-commercial.

## Delivery and caching

The Go API always streams the bytes (proxy mode). Cloudflare caches the responses.

### Request flow

1. A browser requests `https://img.example.com/i/{id}/w_320,f_webp`.
2. On a Cloudflare **hit**, the edge serves the response and picapi is never contacted. It costs nothing.
3. On a **miss**, Cloudflare forwards the request to picapi, which then:
   1. looks up `{id}` in SQLite (404 if missing or deleted; auth check if private),
   2. looks for the variant in `cached/` in the bucket and streams it if it exists,
   3. otherwise reads the original from the bucket, transforms it with libvips, streams the result, and writes it to `cached/` in the background.
4. Cloudflare stores the response according to `Cache-Control`.

### Costs on Railway

- Reading from a Railway Bucket is free (bucket egress and S3 operations are free).
- Bytes the service sends out are **service egress**, about $0.05/GB. That covers responses to Cloudflare, and also uploads into the bucket.
- So you pay once per cache miss, for the transformed bytes only, which are usually tiny compared with the original.

### Why keep `cached/` in the bucket when Cloudflare caches anyway

Cloudflare's cache is not durable storage:

- It evicts objects that are rarely requested.
- Each data center caches independently. Tiered Cache reduces the misses but doesn't remove them.
- Purging (for example after a delete) empties it.

Without `cached/`, every one of those misses means reading the full original (possibly many MB) and transforming it again. Storing a variant costs about $0.015/GB-month plus a one-time upload, so the bucket cache is cheap insurance.

- It is a second-level cache: Cloudflare first, then `cached/`, then transforming the original.
- It is required for background-removal **masks**, because those cost seconds of CPU to compute.
- It can be turned off for other variants with `VARIANT_STORE=off`.
- A periodic janitor deletes variants that haven't been read for `VARIANT_TTL` (default 30 days). Last access is tracked in SQLite, since not every provider supports lifecycle rules.

### Cache headers

Because assets are immutable, every public delivery URL is cached forever.

| Response | `Cache-Control` |
|---|---|
| Public asset or variant | `public, max-age=31536000, immutable` + `ETag` |
| Private asset | `private, max-age=<seconds until token exp>` (never stored by Cloudflare) |
| Redirect to canonical URL | `public, max-age=31536000` |
| Errors | `no-store` |

### Canonical URLs and purging

The same variant can be written several ways (query vs path form, parameter order, `w_320` vs `w_320,q_80` when 80 is the default). To keep exactly one cached copy per variant:

- picapi normalizes the parameters and, if the request isn't already in canonical form, answers with a `301` to the canonical path. Clients (UI, CLI, MCP) always generate canonical URLs, so the redirect is rare.
- Every canonical URL that picapi serves is recorded in the `variants` table, even when `VARIANT_STORE=off`.

The only times cached copies must go away are **delete** and **public → private**. picapi then purges every recorded URL of the asset through the Cloudflare API, in batches (`CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_API_TOKEN`). Without Cloudflare credentials, deleted or now-private images can stay visible at the edge until Cloudflare evicts them. The UI warns about this.

### Cloudflare setup (documented for users)

- Make the DNS record for the image hostname **proxied** (orange cloud), pointing to the Railway service.
- Add a Cache Rule: hostname = image host and path starts with `/i/` → *Eligible for cache*, *respect origin Cache-Control*, and include the query string in the cache key.
- Turn on Tiered Cache (available on the free plan).
- Do not use `f_auto` behind Cloudflare. The free plan ignores `Vary: Accept`, so one format would be served to every browser. Use explicit formats, and `<picture>` when you want to offer several.

## Storage layout

One private bucket. `S3_ENV_PREFIX` (optional) is prepended to every key so several environments can share one bucket.

```
{prefix}originals/{id}                  original bytes, never overwritten
{prefix}cached/{id}/{hash}.{ext}        transformed variants
{prefix}cached/{id}/mask.png            background-removal alpha mask
```

- `{id}`: 12 characters of base62 (`0-9A-Za-z`) from `crypto/rand`, about 71 bits of randomness. It is never reused, so a deleted ID can't later point at different content that an old cache still holds.
  - Why not ULID: a ULID is 26 characters and embeds the upload timestamp in every public URL. Its only advantage, sorting by time, is already covered by the `created_at` column.
  - Collisions are practically impossible at this size, but an insert that hits the primary key retries with a new ID anyway.
- `{hash}`: hash of the **normalized** transform parameters plus `TRANSFORM_VERSION`. Bumping `TRANSFORM_VERSION` (e.g. after an encoder change) invalidates every stored variant without touching originals. The public URL doesn't change, so Cloudflare keeps serving the old bytes until they are evicted or purged. That's acceptable, because the old variant is still a correct rendering of the same image.
- The original's essential metadata (`id`, filename, content type) is also written as S3 object metadata (`x-amz-meta-*`). The database can then be rebuilt from the bucket alone.

## Metadata (SQLite)

The database is the source of truth for what exists; the bucket is the source of truth for bytes.

```sql
CREATE TABLE assets (
  id              TEXT PRIMARY KEY,
  filename     TEXT NOT NULL,
  content_type TEXT NOT NULL,
  size_bytes   INTEGER NOT NULL,
  width        INTEGER,
  height       INTEGER,
  sha256       TEXT NOT NULL,
  visibility   TEXT NOT NULL CHECK (visibility IN ('public', 'private')),
  created_at   TIMESTAMP NOT NULL,
  updated_at   TIMESTAMP NOT NULL,
  deleted_at   TIMESTAMP
);

CREATE TABLE variants (
  asset_id      TEXT NOT NULL REFERENCES assets(id),
  hash          TEXT NOT NULL,
  canonical_url TEXT NOT NULL,    -- for Cloudflare purge
  stored_key    TEXT,             -- NULL when not stored in cached/
  size_bytes    INTEGER,
  last_accessed TIMESTAMP NOT NULL,
  PRIMARY KEY (asset_id, hash)
);

CREATE TABLE api_keys (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  key_hash    TEXT NOT NULL,      -- never store the key itself
  created_at  TIMESTAMP NOT NULL,
  last_used   TIMESTAMP,
  revoked_at  TIMESTAMP
);
```

- `variants.last_accessed` is updated in batches, not on every request.
- On Railway, SQLite lives on a volume. Litestream replication into the bucket is a later option.

### Write ordering and consistency

- **Upload**: write the object first, then commit the row. A crash leaves an orphan object, never a row pointing at nothing.
- **Visibility change**: update the row. Public → private also purges Cloudflare.
- **Delete**: soft-delete the row (requests that reach picapi return 404 immediately), purge Cloudflare, then delete `originals/{id}` and `cached/{id}/` in the background.
- **`picapi reconcile`**: reports, and with `--fix` repairs, orphan objects, rows without objects, and stale variants.

## URL grammar

```
/i/{id}[/{transforms}][/{slug}.{ext}]
```

- `transforms` is a comma-separated list of `key_value` pairs. The same parameters are also accepted as query parameters (`?w=320&f=webp`); non-canonical forms are redirected to the canonical path (see [Canonical URLs and purging](#canonical-urls-and-purging)).
- `slug` is optional and ignored by the server. It exists for readable URLs and download filenames.

Examples:

```
/i/a8Kd93LmQx2Z
/i/a8Kd93LmQx2Z/w_320,f_webp,q_70
/i/a8Kd93LmQx2Z/w_640,h_640,fit_cover,f_avif/team-photo.avif
/i/a8Kd93LmQx2Z/w_480,bg_remove,f_webp?s=<signature>
```

### Transform parameters (v2.0)

| Key | Values | Meaning |
|---|---|---|
| `w`, `h` | pixels | Target width/height. Missing side keeps the aspect ratio. Never upscales unless `up_1`. |
| `fit` | `inside` (default), `cover`, `fill` | `cover` crops to fill the box. |
| `g` | `center` (default), `attention`, `n`, `s`, `e`, `w` | Crop gravity for `fit_cover`. `attention` is libvips smart crop. |
| `f` | `jpeg`, `png`, `webp`, `avif`, `auto` | Output format. `auto` negotiates with `Accept`; only for deployments without a CDN. |
| `q` | 1–100 | Quality for lossy formats. |
| `dpr` | 1, 2, 3 | Multiplies `w`/`h`. |
| `bg` | `remove` | Background removal. Output must support alpha (png/webp/avif), unless `bgc` is set. |
| `bgc` | hex color | Background color to fill after `bg_remove` (allows jpeg). |

Always applied: EXIF orientation is honored and metadata (EXIF/GPS/XMP) is stripped.

Limits: max output 4096×4096, a max decoded input pixel count (decompression-bomb protection), and a bounded number of concurrent transforms.

### Allowed without a signature

To bound how many variants can exist per image, unsigned requests may only use values from the grid below. Anything else is rejected with 400, unless the URL is signed.

- `w`, `h`: 64, 128, 256, 320, 480, 640, 960, 1280, 1920
- `q`: 50, 60, 70, 80, 90
- `f`, `fit`, `g`, `dpr`: any listed value
- Named presets, e.g. `p_thumb` (see [Presets](#presets))
- **Not** `bg_remove`. It always requires a signature or a token, because the first request costs seconds of CPU.

### Presets

Presets are hard-coded in the Go source. They are not stored in the database and are not configurable. Changing one is a code change, and it bumps `TRANSFORM_VERSION` if the output changes.

| Preset | Expands to |
|---|---|
| `p_thumb` | `w_128,h_128,fit_cover,g_attention,f_webp,q_70` |
| `p_small` | `w_320,f_webp,q_75` |
| `p_medium` | `w_640,f_webp,q_80` |
| `p_large` | `w_1280,f_webp,q_80` |
| `p_og` | `w_1200,h_630,fit_cover,g_attention,f_jpeg,q_80` (Open Graph / social previews) |

A preset can be combined with explicit parameters, which override it (e.g. `p_thumb,f_avif`). The canonical URL keeps the preset name rather than its expansion, so the URL stays short.

### Signed URLs

`s` is an HMAC-SHA256 over the normalized path and parameters, using `SIGNING_SECRET`, encoded as base64url. Optional `exp` is a unix timestamp. The UI, CLI and MCP generate signed URLs through the API (`POST /api/v1/assets/{id}/url`).

## Authentication

| Who | Mechanism |
|---|---|
| Management API, CLI, MCP | `Authorization: Bearer <api key>`. Keys are created via `picapi keys create`. The first key comes from `PICAPI_BOOTSTRAP_KEY`. |
| Web UI | Log in with an API key → HttpOnly, Secure, SameSite=Strict session cookie. |
| Viewing private assets | Short-lived JWT (HS256, `SIGNING_SECRET`) in `?token=`, because `<img>` cannot send headers. Claims: `sub` (asset id), `exp`, optional `var` (allowed transforms). |
| Public assets | No auth. Transforms limited to the grid, or signed. |

Private responses are sent with `Cache-Control: private` and `Referrer-Policy: no-referrer`, so tokens in URLs don't leak to third parties through the `Referer` header.

## HTTP API (sketch)

All under `/api/v1`, JSON, API key required.

```
POST   /assets                       upload (multipart); returns asset + URLs
GET    /assets                       list (cursor pagination)
GET    /assets/{id}                  metadata
PATCH  /assets/{id}                  filename, visibility
DELETE /assets/{id}
POST   /assets/{id}/url              build a (signed) delivery URL for given transforms
POST   /assets/{id}/share            mint a viewer JWT for a private asset
GET    /healthcheck                  (outside /api/v1, no auth)
```

Delivery routes (`/i/...`) are outside `/api/v1` so they can be cached separately.

Uploads go through the API at first, so the server can validate the content type (by sniffing the bytes, not trusting the extension), read dimensions and compute the hash. Presigned direct-to-bucket uploads can be added later for large documents.

## Clients

- **CLI** (`picapi`): `upload`, `ls`, `rm`, `url --w 320 --f webp`, `share`, `keys`, `reconcile`, `serve`. It is the same binary as the server, so `serve` starts the server.
- **MCP**: served by the API at `/mcp` (streamable HTTP) and authenticated with an API key. Its tools mirror the API: upload from a URL or base64, list, get URL, remove background, share.
- **Web UI**: browse, upload (drag and drop), delete, toggle visibility, a URL builder with live preview, and copy URL.

## Security notes

- SVG is never transformed or rendered inline. It is served with `Content-Disposition: attachment` and a restrictive CSP to prevent XSS on the image domain.
- Uploads are limited by `MAX_UPLOAD_BYTES`. Content types are allow-listed.
- libvips runs with a decoded-pixel limit and timeouts. Background removal runs behind a semaphore (`BG_CONCURRENCY`, default 1) and `singleflight`, so identical concurrent requests compute once.
- API keys are stored hashed and shown only once.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | — | Required. |
| `S3_USE_PATH_STYLE` | `true` | |
| `S3_ENV_PREFIX` | empty | Key prefix for sharing a bucket between environments. |
| `DATABASE_PATH` | `/data/picapi.db` | SQLite file (on a volume). |
| `DATABASE_URL` | empty | If set, use Postgres instead. |
| `PUBLIC_BASE_URL` | — | e.g. `https://img.example.com`; used when building URLs. |
| `SIGNING_SECRET` | — | Required. HMAC for signed URLs and JWTs. |
| `PICAPI_BOOTSTRAP_KEY` | empty | Creates the first API key on startup if none exist. |
| `VARIANT_STORE` | `on` | Store variants in `cached/`. |
| `VARIANT_TTL` | `720h` | Janitor deletes variants not read for this long. |
| `TRANSFORM_VERSION` | built-in | Override to force-invalidate all variants. |
| `MAX_UPLOAD_BYTES` | `52428800` | 50 MB. |
| `TRANSFORM_CONCURRENCY` | number of CPUs | |
| `BG_MODEL` | `isnet` | `u2netp`, `isnet`, `birefnet`, `off`. |
| `BG_CONCURRENCY` | `1` | |
| `CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_API_TOKEN` | empty | Enables purging on delete and public → private. |

## Deployment

Reference deployment: **Railway service (Docker image) + Railway volume (SQLite) + Railway Bucket**, with the custom domain on Cloudflare proxied to the Railway service. A Railway template is planned.

Any other host works as long as it can run the container and reach an S3-compatible bucket. Hosting the bucket in the same provider and region as the service keeps transforms on cache misses fast.

## Implementation status

Milestone 1 lives in `api-go/` (the earlier prototype in `api/` is superseded). Done:

- Config, S3 storage, SQLite with migrations, bootstrap API key.
- Management API: upload, list, get, patch, delete, `url`, `share`.
- Delivery: originals and variants, presets, grid and signed URLs, canonical redirects, private assets with JWTs, ETag/304, variant cache in `cached/`, `singleflight` + concurrency limit.
- Debug headers on delivery: `X-Picapi-Variant` (hash), `X-Picapi-Cache` (`hit`/`miss`), `Server-Timing` (render time).

Not yet: background removal (returns 501), Cloudflare purge and the URL table it needs, variant janitor, `reconcile`, CLI, MCP, web UI.

The variant table currently stores only stored variants; the canonical URL list for purging will be added together with purge.

### Local development and tests

```
docker compose up -d --build        # SeaweedFS (S3) on :8333, API on :8081
cd conformance && npm install && npm run test:local
```

`conformance/` is a black-box test suite run against a running instance (`BASE_URL`, `API_KEY`). It is test tooling only; it uses Node and sharp to generate and inspect images.

## Later

- **Video posters**: an `ffmpeg` subprocess extracts a frame. The poster becomes an image, so all image transforms apply.
- **PDF thumbnails**: `klippa-app/go-pdfium` in WebAssembly mode (no cgo) renders the first page.
- Presigned direct uploads for large files.
- Litestream replication of SQLite into the bucket.
- `-slim` Docker image without ONNX Runtime and models.

## Open questions

None at the moment.
