# picapi v2 — Architecture

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
                       ┌───────────────────────────── Railway ─────────────────────────────┐
browser / <img> ──▶ Cloudflare CDN ──miss──▶ picapi-server (Go) ──▶ Railway Bucket (private, S3)
                    (edge cache)            │  ├─ libvips (govips)                          │
CLI / MCP / UI ─────────────────────────────▶  ├─ ONNX Runtime (background removal)         │
                                            │  └──────────────▶ Postgres (metadata)         │
                       └───────────────────────────────────────────────────────────────────┘
```

- **picapi-server** is a single Go service. It runs the management API and the MCP endpoint, delivers images, and serves the embedded web UI.
- **Object storage** is any S3-compatible bucket and is always private.
- **Postgres** holds metadata and is the source of truth for which assets exist. The server is stateless: no volume, and it can be redeployed freely.
- **Cloudflare** sits in front of the public hostname as a reverse proxy/CDN, with the DNS record proxied (orange cloud). It is the cache users actually hit.
- **picapi** (CLI) is a separate, pure-Go binary that talks to the HTTP API. It needs no cgo or libvips, so it builds as a single static file.

## Tech stack

| Concern | Choice | Notes |
|---|---|---|
| Language | Go | |
| HTTP | Echo v5 | |
| Image processing | libvips via `govips` | Shrink-on-load, smart crop, JPEG/PNG/WebP/AVIF/GIF. Requires cgo. |
| Background removal | ONNX Runtime 1.31 via `yalue/onnxruntime_go` | Same models and pre/post-processing as rembg, ported to Go. |
| Metadata | Postgres via `pgx/v5` | Migrations are embedded SQL files, applied at startup under an advisory lock. |
| Object storage | `aws-sdk-go-v2` S3 client | Checksums are only sent when required (R2 compatibility). |
| MCP | Official MCP Go SDK | Streamable HTTP at `/mcp`, stateless. |
| Web UI | Vanilla JS, embedded with `embed.FS` | Served at `/`. |
| Packaging | Docker image | libvips, ONNX Runtime and both models are baked in. Contains `picapi-server` and `picapi`. |

### Code layout

```
api-go/
  cmd/picapi-server/   server entry point
  cmd/picapi/          CLI (CGO_ENABLED=0)
  internal/server/     HTTP API, delivery, transforms, background removal, MCP, UI, maintenance
  internal/server/migrations/   Postgres schema
  internal/server/ui/  web UI (HTML, CSS, JS)
  internal/client/     HTTP client used by the CLI
  Dockerfile           build context is the repository root
conformance/           black-box HTTP test suite
docker-compose.yml     local SeaweedFS (S3) + Postgres + server
```

### Background-removal models

Only permissively licensed models are shipped. Both are in the Docker image.

| `BG_MODEL` | License | Size | Notes |
|---|---|---|---|
| `isnet` (default) | Apache-2.0 | ~170 MB | IS-Net general use, 1024×1024 input. Good quality; about 3 s per image on a small Railway instance. |
| `u2netp` | Apache-2.0 | ~4.5 MB | 320×320 input. Fast, lower quality; used for local development. |
| `off` | — | — | `bg_remove` returns 501. |

BRIA RMBG models are not supported because their license is non-commercial. BiRefNet (MIT) may be added later.

The model and ONNX Runtime load on the first background removal, so servers that never use it don't pay the memory. At most `BG_CONCURRENCY` (default 1) model runs happen at once.

## Delivery and caching

The Go server always streams the bytes (proxy mode). Cloudflare caches the responses.

### Request flow

1. A browser requests `https://img.example.com/i/{id}/w_320,f_webp`.
2. On a Cloudflare **hit**, the edge serves the response and picapi is never contacted. It costs nothing.
3. On a **miss**, Cloudflare forwards the request to picapi, which then:
   1. looks up `{id}` in Postgres (404 if missing or deleted; auth check if private),
   2. looks for the variant in `cached/` in the bucket and streams it if it exists,
   3. otherwise reads the original from the bucket, transforms it with libvips, streams the result, and writes it to `cached/` in the background.
4. Cloudflare stores the response according to `Cache-Control`.

Concurrent requests for the same variant share one render (`singleflight`), and at most `TRANSFORM_CONCURRENCY` renders run at once.

Every delivery response carries debug headers: `X-Picapi-Variant` (variant hash), `X-Picapi-Cache` (`hit` from the bucket, or `miss`), and on misses `Server-Timing: transform;dur=<ms>`.

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
- Background-removal **masks** are always stored, because they cost seconds of CPU to compute.
- Other variants can be turned off with `VARIANT_STORE=off`.
- A janitor runs every `JANITOR_INTERVAL` (default 1 h) and deletes variants that haven't been read for `VARIANT_TTL` (default 30 days). Last access is tracked in Postgres, since not every provider supports lifecycle rules.

### Cache headers

Because assets are immutable, every public delivery URL is cached forever.

| Response | `Cache-Control` |
|---|---|
| Public asset or variant | `public, max-age=31536000, immutable` + `ETag` |
| Public, signed URL with `exp` | `public, max-age=<seconds until exp>` |
| Private asset with a view token | `private, max-age=<seconds until token exp>` (never stored by Cloudflare) |
| Private asset seen through the web UI's session cookie | `private, max-age=300` |
| Redirect to canonical URL | `public, max-age=31536000` (private assets: `no-store`) |
| Errors | `no-store` |

### Canonical URLs and purging

The same variant can be written several ways (query vs path form, parameter order). To keep exactly one cached copy per URL form:

- picapi parses the parameters and, if the request isn't already in canonical form, answers with a `301` to the canonical path. Canonical form is the path form with keys in a fixed order (`p, w, h, fit, g, f, q, dpr, up, bg, bgc`) and the query reduced to `exp`, `s`, `token`. Clients (UI, CLI, MCP) always generate canonical URLs, so the redirect is rare.
- Every public URL picapi serves with a long cache lifetime is recorded in `served_urls` (once per process per URL).

The only times cached copies must go away are **delete** and **public → private**. picapi then purges every recorded URL of the asset through the Cloudflare API, in batches of 30 (`CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_API_TOKEN`). Without Cloudflare credentials, deleted or now-private images can stay visible at the edge until Cloudflare evicts them.

### Cloudflare setup

- Make the DNS record for the image hostname **proxied** (orange cloud), pointing to the Railway service. SSL/TLS mode must be *Full* (Railway serves HTTPS).
- Add a Cache Rule: hostname equals the image host and URI path starts with `/i/` → *Eligible for cache*, edge TTL *use cache-control header if present*, browser TTL *respect origin*. Cloudflare's default cache key includes the full query string, which signed URLs need.
- Turn on Tiered Cache (available on the free plan).
- `f_auto` is deliberately not offered: the free plan ignores `Vary: Accept`, so one format would be served to every browser. Use explicit formats, and `<picture>` to offer several.
- For purging, create a token with only *Zone → Cache Purge → Purge* and set `CLOUDFLARE_ZONE_ID` and `CLOUDFLARE_API_TOKEN`.

## Storage layout

One private bucket. `S3_ENV_PREFIX` (optional) is prepended to every key so several environments can share one bucket.

```
{prefix}originals/{id}                             original bytes, never overwritten
{prefix}cached/{id}/{hash}.{ext}                   transformed variants
{prefix}cached/{id}/mask-{model}-v{version}.png    background-removal alpha mask
```

- `{id}`: 12 characters of base62 (`0-9A-Za-z`) from `crypto/rand`, about 71 bits of randomness. It is never reused, so a deleted ID can't later point at different content that an old cache still holds.
  - Why not ULID: a ULID is 26 characters and embeds the upload timestamp in every public URL. Its only advantage, sorting by time, is already covered by the `created_at` column.
- `{hash}`: first 32 hex characters of SHA-256 over `TRANSFORM_VERSION`, the asset ID and the fully resolved parameters (presets expanded, defaults filled in). Bumping `TRANSFORM_VERSION` (e.g. after an encoder change) invalidates every stored variant and mask without touching originals. The public URL doesn't change, so Cloudflare keeps serving the old bytes until they are evicted or purged. That's acceptable, because the old variant is still a correct rendering of the same image.
- The original's ID and filename are also written as S3 object metadata (`x-amz-meta-picapi-*`).

## Metadata (Postgres)

The database is the source of truth for what exists; the bucket is the source of truth for bytes. The schema lives in `api-go/internal/server/migrations/`.

| Table | Purpose |
|---|---|
| `assets` | One row per upload: filename, content type, size, display dimensions, SHA-256, visibility, timestamps, `deleted_at` (soft delete). |
| `variants` | Variants stored in `cached/`: key, content type, size, `last_accessed` (for the janitor). |
| `served_urls` | Public URLs served with long cache lifetimes, for Cloudflare purging. |
| `api_keys` | Name, SHA-256 of the key (the key itself is never stored), `last_used`, `revoked_at`. |
| `schema_migrations` | Applied migration versions. |

### Write ordering and consistency

- **Upload**: write the object first, then commit the row. A crash leaves an orphan object, never a row pointing at nothing.
- **Visibility change**: update the row. Public → private also purges Cloudflare, in the background.
- **Delete**: soft-delete the row (requests that reach picapi return 404 immediately), then in the background purge Cloudflare and delete `originals/{id}` and `cached/{id}/`.
- **Variant store**: the object is written, then the row is inserted only if the asset still exists; otherwise the object is deleted again.
- **Reconcile** (`picapi reconcile`, `POST /api/v1/admin/reconcile`): reports, and with `--fix` repairs, orphan originals, orphan variants and stale variant rows. Live assets whose original is missing are reported only.

## URL grammar

```
/i/{id}[/{transforms}][/{slug}.{ext}]
```

- `transforms` is a comma-separated list of `key_value` pairs. The same parameters are also accepted as query parameters (`?w=320&f=webp`); non-canonical forms are redirected to the canonical path.
- `slug` is optional and ignored by the server. It exists for readable URLs and download filenames. It must contain a dot (`name.ext`), which is how it is told apart from transforms.

Examples:

```
/i/a8Kd93LmQx2Z
/i/a8Kd93LmQx2Z/w_320,f_webp,q_70
/i/a8Kd93LmQx2Z/w_640,h_640,fit_cover,f_avif/team-photo.avif
/i/a8Kd93LmQx2Z/w_480,f_webp,bg_remove?s=<signature>
```

### Transform parameters

| Key | Values | Meaning |
|---|---|---|
| `p` | `thumb`, `small`, `medium`, `large`, `og` | Preset; explicit keys override it. |
| `w`, `h` | 1–4096 | Target width/height. A missing side keeps the aspect ratio. Never upscales unless `up_1`. |
| `fit` | `inside` (default), `cover`, `fill` | `cover` crops to fill the box; `cover` and `fill` need both `w` and `h`. |
| `g` | `center` (default), `attention`, `n`, `s`, `e`, `w` | Crop gravity, only with `fit_cover`. `attention` is libvips smart crop. |
| `f` | `jpeg`, `png`, `webp`, `avif` | Output format. Default: the original's format (GIF becomes PNG). |
| `q` | 1–100 | Quality for lossy formats (default 80). Not allowed with PNG output. |
| `dpr` | 1, 2, 3 | Multiplies `w`/`h`; the result must stay ≤ 4096. |
| `up` | 0, 1 | Allow upscaling. |
| `bg` | `remove` | Background removal. Output must support alpha (png/webp/avif), unless `bgc` is set. |
| `bgc` | 6-digit lowercase hex | Background color to fill after `bg_remove` (allows jpeg). |

Always applied: EXIF orientation is honored, output is sRGB, and metadata (EXIF/GPS/XMP) is stripped. JPEG output flattens transparency onto white. Originals (`/i/{id}` with no transforms) are served byte for byte as uploaded.

Limits: max output 4096×4096, `MAX_INPUT_PIXELS` decoded input pixels (decompression-bomb protection), and `MAX_UPLOAD_BYTES` per upload.

### Allowed without a signature

To bound how many variants can exist per image, unsigned requests may only use values from the grid below. Anything else returns 403 unless the URL is signed.

- `w`, `h`: 64, 128, 256, 320, 480, 640, 960, 1280, 1920
- `q`: 50, 60, 70, 80, 90
- `f`, `fit`, `g`, `dpr`, `up`: any listed value
- Presets, even where their values are off the grid (e.g. `p_og` is 1200×630)
- **Not** `bg_remove`. It always requires a signature, because the first request costs seconds of CPU.

### Presets

Presets are hard-coded in the Go source. They are not stored in the database and are not configurable. Changing one is a code change, and it bumps `TRANSFORM_VERSION` if the output changes.

| Preset | Expands to |
|---|---|
| `p_thumb` | `w_128,h_128,fit_cover,g_attention,f_webp,q_70` |
| `p_small` | `w_320,f_webp,q_75` |
| `p_medium` | `w_640,f_webp,q_80` |
| `p_large` | `w_1280,f_webp,q_80` |
| `p_og` | `w_1200,h_630,fit_cover,g_attention,f_jpeg,q_80` (Open Graph / social previews) |

The canonical URL keeps the preset name rather than its expansion, so the URL stays short.

### Signed URLs

`s` is the base64url HMAC-SHA256 (with `SIGNING_SECRET`) of the path without slug, `/i/{id}/{canonical transforms}`, followed by `?exp={exp}` when the URL expires. The UI, CLI and MCP get signed URLs from `POST /api/v1/assets/{id}/url`, which signs automatically when the transforms need it.

## Authentication

| Who | Mechanism |
|---|---|
| Management API, CLI, MCP | `Authorization: Bearer <api key>`. The first key comes from `PICAPI_BOOTSTRAP_KEY` (created only when no keys exist); more keys via `picapi keys create`. Keys look like `pk_` + 40 base62 characters. |
| Web UI | `POST /api/v1/session` with an API key → HttpOnly, SameSite=Strict session cookie (Secure on HTTPS), valid 30 days or until the key is revoked. Cookie-authenticated writes must send `X-Picapi-CSRF: 1`. |
| Viewing private assets | Short-lived JWT (HS256, `SIGNING_SECRET`, claims `sub` = asset ID, `exp`) in `?token=`, because `<img>` cannot send headers. The web UI's session cookie also works. |
| Public assets | No auth. Transforms limited to the grid, or signed. |

Private responses carry `Referrer-Policy: no-referrer`, and tokens are redacted from request logs.

## HTTP API

All under `/api/v1`, JSON, authenticated (see above).

```
POST   /session                      log in with {"key": ...} → cookie (no auth needed)
DELETE /session                      log out
GET    /session                      who am I

POST   /assets                       upload (multipart: file, optional visibility, filename) → 201 asset
GET    /assets?limit=&cursor=        list, newest first → {items, next_cursor}
GET    /assets/{id}                  metadata
PATCH  /assets/{id}                  {filename?, visibility?}
DELETE /assets/{id}                  → 204
POST   /assets/{id}/url              {transforms?, slug?, expires_in?} → {url}, signed when needed
POST   /assets/{id}/share            {transforms?, slug?, expires_in?} → {url, token, expires_at}

GET    /keys                         list keys (never the key values)
POST   /keys                         {name} → key, shown once
DELETE /keys/{id}                    revoke (the last active key can't be revoked)

POST   /admin/reconcile?fix=true     consistency report / repair
```

Outside `/api/v1`: `GET /healthcheck`, delivery at `/i/...`, MCP at `/mcp`, web UI at `/`.

Errors are always `{"error": "..."}` with `Cache-Control: no-store`.

## Clients

- **CLI** (`picapi`): `login`, `upload`, `ls`, `info`, `set`, `rm`, `url`, `share`, `keys`, `reconcile`. Credentials come from `PICAPI_URL`/`PICAPI_KEY` or from the config file written by `picapi login`.
- **MCP** (`/mcp`, Bearer API key): tools `list_assets`, `get_asset`, `upload_from_url`, `upload_data`, `update_asset`, `delete_asset`, `get_url`, `share_asset`, `remove_background`. `upload_from_url` refuses private, loopback and link-local addresses, so it can't reach internal services.
- **Web UI** (`/`): log in with an API key, browse, upload (button or drag and drop), rename, toggle visibility, delete, a URL builder with live preview and copy, and 24-hour share links.

## Security notes

- SVG and other non-raster types are rejected at upload. The type is sniffed from the bytes, not trusted from the filename.
- The web UI is served with a strict CSP (`default-src 'self'`), `X-Frame-Options: DENY` and `nosniff`.
- API keys are stored as SHA-256 hashes and shown only once.
- libvips runs with a decoded-pixel limit; renders and model runs are bounded by semaphores.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_URL` | — | Required. Postgres connection string. |
| `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | — | Required. |
| `S3_USE_PATH_STYLE` | `true` | Railway Buckets use virtual-hosted style: set `false`. |
| `S3_ENV_PREFIX` | empty | Key prefix for sharing a bucket between environments. |
| `PUBLIC_BASE_URL` | — | Required, e.g. `https://img.example.com`; used when building URLs. |
| `SIGNING_SECRET` | — | Required, ≥ 32 characters. HMAC for signed URLs, JWTs and sessions. |
| `PICAPI_BOOTSTRAP_KEY` | empty | Creates the first API key on startup if none exist. |
| `PORT` | `8080` | |
| `VARIANT_STORE` | `true` | Store variants in `cached/`. |
| `VARIANT_TTL` | `720h` | Janitor deletes variants not read for this long. |
| `JANITOR_INTERVAL` | `1h` | |
| `TRANSFORM_VERSION` | `1` | Bump to invalidate all stored variants and masks. |
| `MAX_UPLOAD_BYTES` | `52428800` | 50 MB. |
| `MAX_INPUT_PIXELS` | `100000000` | |
| `TRANSFORM_CONCURRENCY` | number of CPUs | |
| `BG_MODEL` | `isnet` | `isnet`, `u2netp`, `off`. |
| `BG_CONCURRENCY` | `1` | |
| `BG_MODEL_DIR`, `ONNXRUNTIME_LIB` | image paths | Only needed outside the Docker image. |
| `CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_API_TOKEN` | empty | Enables purging. Set both or neither. |

## Deployment

### Railway (reference deployment)

Project `picapi-v2`, everything in EU West (Amsterdam):

| Service | What |
|---|---|
| `picapi` | Built from `api-go/Dockerfile` with the repository root as build context (`RAILWAY_DOCKERFILE_PATH=api-go/Dockerfile`). No volume. |
| `Postgres` | Railway Postgres. `DATABASE_URL=${{Postgres.DATABASE_URL}}` (private network). |
| `picapi-images` | Railway Bucket. `S3_*` variables reference `${{picapi-images.ENDPOINT}}`, `REGION`, `BUCKET`, `ACCESS_KEY_ID`, `SECRET_ACCESS_KEY`. |

`img.janharkonen.fi` is a proxied Cloudflare CNAME to the Railway service, with a TXT record for Railway's domain verification. `SIGNING_SECRET` and `PICAPI_BOOTSTRAP_KEY` are generated once and stored only as Railway variables.

Deploy with `railway up --service picapi` from the repository root, or connect the GitHub repository with the same Dockerfile path.

### Elsewhere

Any host that can run the container, reach Postgres and reach an S3-compatible bucket works. Hosting the bucket in the same provider and region as the server keeps cache misses fast.

## Local development and tests

```
docker compose up -d --build        # SeaweedFS (S3) on :8333, Postgres on :5433, server on :8081
cd conformance && npm install && npm run test:local
```

`conformance/` is a black-box test suite run against a running instance (`BASE_URL`, `API_KEY`). It is test tooling only; it uses Node and sharp to generate and inspect images. The CLI can be built natively with `cd api-go && CGO_ENABLED=0 go build ./cmd/picapi`.

## Later

- **Video posters**: an `ffmpeg` subprocess extracts a frame. The poster becomes an image, so all image transforms apply.
- **PDF thumbnails**: `klippa-app/go-pdfium` in WebAssembly mode (no cgo) renders the first page.
- Presigned direct uploads for large files.
- BiRefNet as a higher-quality background-removal model.
- A `-slim` Docker image without ONNX Runtime and models.
- Prebuilt CLI binaries on GitHub releases.
