# picapi v2 — Architecture

## Goals

- Upload a picture and get a short, permanent, public URL for it.
- Transform pictures on the fly with URL parameters: resize, crop, format, quality, background removal.
- Keep originals untouched; serve small, optimized variants to minimize egress and page weight.
- Make Cloudflare's cache do the work: every response is public, immutable, and has exactly one canonical URL.
- Self-hosted and open source: each user or project deploys their own single-tenant instance on their own domain.
- Clients: web UI, HTTP API, CLI, and MCP.

## Non-goals (for now)

- Private pictures. Everything is public.
- A database. The bucket is the only store.
- Storing transformed variants. They are rendered on demand and cached by Cloudflare.
- Replacing a picture under the same ID. Pictures are immutable: upload a new one and delete the old one.
- Video and PDF.

## Overview

```
                                     ┌──────────────── Railway ────────────────┐
browser / <img> ──▶ Cloudflare CDN ──miss──▶ picapi-server (Go) ──▶ Railway Bucket
                    (edge cache)     │        ├─ libvips (govips)              │
CLI / MCP / UI ──────────────────────▶        └─ ONNX Runtime (bg removal)     │
                                     └─────────────────────────────────────────┘
```

- **picapi-server** is a single, stateless Go service. It delivers pictures, runs the management API and the MCP endpoint, and serves the embedded web UI.
- **The bucket** is any S3-compatible store and is the only storage. Each picture is one object, `public/{id}`, whose metadata is stored on the object itself.
- **Cloudflare** sits in front of the public hostname as a reverse proxy/CDN (DNS record proxied, orange cloud). It is the cache users actually hit.
- **picapi** (CLI) is a separate, pure-Go binary that talks to the HTTP API. It needs no cgo or libvips.

## Request flow

1. A browser requests `https://img.example.com/i/{id}/w_320,f_webp`.
2. On a Cloudflare **hit**, the edge serves the response. picapi is never contacted.
3. On a **miss**, Cloudflare forwards the request to picapi, which reads `public/{id}` from the bucket and then:
   - **no transforms**: streams the object's bytes straight to the client;
   - **transforms**: renders the variant in memory with libvips and streams the result.
4. Cloudflare stores the response, which is marked `public, max-age=31536000, immutable`.

Nothing is written on delivery. Concurrent requests for the same variant share one render (`singleflight`), and at most `TRANSFORM_CONCURRENCY` renders run at once. Background removal recomputes its mask on every render, so its first request per URL takes a few seconds; after that Cloudflare serves it.

Delivery responses carry `X-Picapi-Variant` (variant hash) and, for transforms, `Server-Timing: transform;dur=<ms>`.

## Optimizing Cloudflare caching

Every design choice below exists so that one picture variant is one cached object at the edge, for as long as possible.

- **Immutable assets.** A picture never changes under its ID, so every response can be cached for a year with `immutable`.
- **One canonical URL per variant.** The same variant can be written several ways (query vs path form, parameter order, extra query parameters). picapi answers anything non-canonical with a `301` (itself cacheable) to the canonical path: path form, keys in the fixed order `p, w, h, fit, g, f, q, dpr, up, bg, bgc`, and a query reduced to `exp` and `s`. Clients (UI, CLI, MCP) always generate canonical URLs.
- **A bounded variant space.** Unsigned URLs may only use a grid of sizes and qualities and the presets, so the number of distinct URLs per picture (and so cache misses and renders) stays small. Anything else must be signed.
- **No content negotiation.** There is no `f_auto`: Cloudflare's free plan ignores `Vary: Accept`, so negotiated formats would poison the cache. Use explicit formats, and `<picture>` to offer several.
- **Validators.** Responses have `ETag`s (SHA-256 of the original, or the variant hash), so revalidation after eviction is a cheap `304`.
- **Purge on delete.** Deleting a picture purges `host/i/{id}` by prefix through the Cloudflare API, covering the original and every variant, slug and query form, without tracking URLs. IDs have a fixed length, so the prefix can't match another picture.

### Cache headers

| Response | `Cache-Control` |
|---|---|
| Original or variant | `public, max-age=31536000, immutable` + `ETag` |
| Signed URL with `exp` | `public, max-age=<seconds until exp>` |
| Redirect to canonical URL | `public, max-age=31536000` |
| Errors | `no-store` |

### Cloudflare setup

- DNS record for the image hostname: **proxied** CNAME to the Railway service. SSL/TLS mode *Full*.
- A Cache Rule: hostname equals the image host and URI path starts with `/i/` → *Eligible for cache*, edge TTL *use cache-control header if present*, browser TTL *respect origin*. Without it Cloudflare doesn't cache extensionless paths like `/i/{id}/w_320`. Its default cache key includes the full query string, which signed URLs need.
- Tiered Cache (free plan: Smart Tiered Cache) to reduce misses across data centers.
- For purging on delete: a token with only *Zone → Cache Purge → Purge*, set as `CLOUDFLARE_API_TOKEN` with `CLOUDFLARE_ZONE_ID`.

### Costs on Railway

- Reading from a Railway Bucket is free (no bucket egress or operation fees).
- Bytes the service sends out are service egress, about $0.05/GB. With Cloudflare caching, that is paid roughly once per variant per data center, not per view.

## Storage

One private bucket. `S3_ENV_PREFIX` (optional) is prepended so several environments can share one bucket.

```
{prefix}public/{id}
```

| Field | Where |
|---|---|
| Content type | `Content-Type` of the object (sniffed from the bytes at upload) |
| Filename | `x-amz-meta-filename` (URL-escaped) |
| Width, height (after EXIF orientation) | `x-amz-meta-width`, `x-amz-meta-height` |
| SHA-256 | `x-amz-meta-sha256` |
| Created at | the object's `LastModified` (one-second resolution) |

- `{id}`: 12 characters of base62 from `crypto/rand` (about 71 bits). IDs are never reused, so a deleted ID can't point at different content that an old cache still holds.
- Listing reads the bucket listing, sorts it newest first by `LastModified`, and HEADs the objects on the requested page for their metadata. That is fine for the hundreds-to-thousands of pictures a single-tenant instance holds.

## URL grammar

```
/i/{id}[/{transforms}][/{slug}.{ext}]
```

- `transforms` is a comma-separated list of `key_value` pairs. The query form (`?w=320&f=webp`) is redirected to the path form.
- `slug` is optional and ignored by the server; it gives readable URLs and download names. It must contain a dot, which is how it is told apart from transforms.

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

Always applied to variants: EXIF orientation is honored, output is sRGB, and metadata (EXIF/GPS/XMP) is stripped. JPEG output flattens transparency onto white. Originals are served byte for byte as uploaded.

Limits: output ≤ 4096×4096, `MAX_INPUT_PIXELS` decoded pixels, `MAX_UPLOAD_BYTES` per upload.

### Allowed without a signature

- `w`, `h`: 64, 128, 256, 320, 480, 640, 960, 1280, 1920
- `q`: 50, 60, 70, 80, 90
- `f`, `fit`, `g`, `dpr`, `up`: any listed value
- Presets, even where their values are off the grid
- **Not** `bg_remove`, which always needs a signature because it costs seconds of CPU.

Anything else returns 403 unless signed.

### Presets

Hard-coded in the Go source. Changing one bumps `TRANSFORM_VERSION` if the output changes.

| Preset | Expands to |
|---|---|
| `p_thumb` | `w_128,h_128,fit_cover,g_attention,f_webp,q_70` |
| `p_small` | `w_320,f_webp,q_75` |
| `p_medium` | `w_640,f_webp,q_80` |
| `p_large` | `w_1280,f_webp,q_80` |
| `p_og` | `w_1200,h_630,fit_cover,g_attention,f_jpeg,q_80` |

### Signed URLs

`s` is the base64url HMAC-SHA256 (with `SIGNING_SECRET`) of `/i/{id}/{canonical transforms}` (no slug), followed by `?exp={exp}` when the URL expires. `POST /api/v1/assets/{id}/url` signs automatically when needed.

### Variant hash

`X-Picapi-Variant` and variant ETags are the first 32 hex characters of SHA-256 over `TRANSFORM_VERSION`, the ID and the fully resolved parameters (presets expanded, defaults filled in).

## Background removal

ONNX Runtime 1.31 via `yalue/onnxruntime_go`, with the same models and pre/post-processing as rembg. Both models are in the Docker image and load on first use.

| `BG_MODEL` | License | Size | Notes |
|---|---|---|---|
| `isnet` (default) | Apache-2.0 | ~170 MB | IS-Net general use, 1024×1024 input. About 3 s per picture on a small Railway instance. |
| `u2netp` | Apache-2.0 | ~4.5 MB | 320×320 input. Fast, lower quality; used for local development. |
| `off` | — | — | `bg_remove` returns 501. |

At most `BG_CONCURRENCY` (default 1) model runs happen at once.

## Authentication

| Who | Mechanism |
|---|---|
| Management API, CLI, MCP | `Authorization: Bearer <PICAPI_API_KEY>`. One key per instance; rotate it by changing the variable. |
| Web UI | `POST /api/v1/session` with the key → HttpOnly, SameSite=Strict session cookie (Secure on HTTPS), valid 30 days. The cookie is bound to the current key, so changing the key ends all sessions. Cookie-authenticated writes must send `X-Picapi-CSRF: 1`. |
| Viewing pictures | No auth. |

## HTTP API

All under `/api/v1`, JSON, authenticated.

```
POST   /session                      log in with {"key": ...} → cookie (no auth needed)
DELETE /session                      log out
GET    /session                      check authentication

POST   /assets                       upload (multipart: file, optional filename) → 201 asset
GET    /assets?limit=&cursor=        list, newest first → {items, next_cursor}
GET    /assets/{id}                  metadata
DELETE /assets/{id}                  delete and purge from Cloudflare → 204
POST   /assets/{id}/url              {transforms?, slug?, expires_in?} → {url}, signed when needed
```

Outside `/api/v1`: `GET /healthcheck`, delivery at `/i/...`, MCP at `/mcp`, web UI at `/`. Errors are always `{"error": "..."}` with `Cache-Control: no-store`.

## Clients

- **CLI** (`picapi`): `login`, `upload`, `ls`, `info`, `rm`, `url`. Credentials come from `PICAPI_URL`/`PICAPI_KEY` or from the config file written by `picapi login`.
- **MCP** (`/mcp`, Bearer key): `list_assets`, `get_asset`, `upload_from_url`, `upload_data`, `delete_asset`, `get_url`, `remove_background`. `upload_from_url` refuses private, loopback and link-local addresses.
- **Web UI** (`/`): log in with the key, browse, upload (button or drag and drop), delete, and a URL builder with live preview and copy.

## Code layout

```
api-go/
  cmd/picapi-server/   server entry point
  cmd/picapi/          CLI (CGO_ENABLED=0)
  internal/server/     delivery, transforms, background removal, API, MCP, UI, purge, storage
  internal/server/ui/  web UI (HTML, CSS, JS)
  internal/client/     HTTP client used by the CLI
  Dockerfile           build context is the repository root
conformance/           black-box HTTP test suite
docker-compose.yml     local SeaweedFS (S3) + server
```

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `PICAPI_API_KEY` | — | Required, ≥ 32 characters. |
| `SIGNING_SECRET` | — | Required, ≥ 32 characters. HMAC for signed URLs and sessions. |
| `PUBLIC_BASE_URL` | — | Required, e.g. `https://img.example.com`. |
| `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | — | Required. |
| `S3_USE_PATH_STYLE` | `true` | Railway Buckets use virtual-hosted style: set `false`. |
| `S3_ENV_PREFIX` | empty | Key prefix for sharing a bucket between environments. |
| `PORT` | `8080` | |
| `TRANSFORM_VERSION` | `1` | Part of the variant hash/ETag. |
| `MAX_UPLOAD_BYTES` | `52428800` | 50 MB. |
| `MAX_INPUT_PIXELS` | `100000000` | |
| `TRANSFORM_CONCURRENCY` | number of CPUs | |
| `BG_MODEL` | `isnet` | `isnet`, `u2netp`, `off`. |
| `BG_CONCURRENCY` | `1` | |
| `BG_MODEL_DIR`, `ONNXRUNTIME_LIB` | image paths | Only needed outside the Docker image. |
| `CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_API_TOKEN` | empty | Enables purge on delete. Set both or neither. |

## Deployment

### Railway (reference deployment)

Project `picapi-v2`, in EU West (Amsterdam):

| Service | What |
|---|---|
| `picapi` | Built from `api-go/Dockerfile` with the repository root as build context (`RAILWAY_DOCKERFILE_PATH=api-go/Dockerfile`). Stateless, no volume. |
| `picapi-images` | Railway Bucket. `S3_*` variables reference `${{picapi-images.ENDPOINT}}`, `REGION`, `BUCKET`, `ACCESS_KEY_ID`, `SECRET_ACCESS_KEY`. |

`img.janharkonen.fi` is a proxied Cloudflare CNAME to the Railway service, with a TXT record for Railway's domain verification. `PICAPI_API_KEY` and `SIGNING_SECRET` are stored only as Railway variables.

Deploy with `railway up --service picapi` from the repository root, or connect the GitHub repository with the same Dockerfile path.

## Local development and tests

```
docker compose up -d --build        # SeaweedFS (S3) on :8333, server on :8081
cd conformance && npm install && npm run test:local
```

`conformance/` is a black-box test suite run against a running instance (`BASE_URL`, `API_KEY`). It uses Node and sharp to generate and inspect images. The CLI builds natively with `cd api-go && CGO_ENABLED=0 go build ./cmd/picapi`.

## Later

- Video posters (ffmpeg) and PDF thumbnails (go-pdfium).
- Presigned direct uploads for large files.
- BiRefNet as a higher-quality background-removal model.
- Prebuilt CLI binaries on GitHub releases.
