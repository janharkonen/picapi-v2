# Architecture

picapi serves pictures stored in an S3-compatible bucket over HTTP. It is
deliberately minimal: one Go service that fetches an object from the bucket
and streams the bytes to the client.

## Overview

```
client ──HTTPS──▶ picapi (Go API, Gin) ──S3 GetObject──▶ bucket (picapi-images)
        ◀── bytes streamed through ─────◀── object body ──
```

- **No caching.** Nothing is cached in a CDN, in the bucket, or in memory.
  Every request results in one `GetObject` call to the bucket.
- **No transformations.** Objects are returned exactly as stored.
- **No authentication.** All objects in the bucket are publicly readable
  through the API.
- **No database.** The bucket is the only state.

## Components

### Go API (`api/`)

A single `main.go` using the [Gin](https://gin-gonic.com) framework and the
AWS SDK for Go v2 S3 client.

| Route        | Behaviour |
|--------------|-----------|
| `GET /*key`  | Fetches object `key` from the bucket and streams it to the client. |

Response details:

- `200` with the object body. `Content-Type`, `Content-Length`, `ETag` and
  `Last-Modified` are copied from the bucket's response.
- `404` if the key is empty or the object does not exist.
- `502` for any other storage error.

The body is streamed with `io.Copy` semantics (Gin `DataFromReader`); the API
never buffers a whole object in memory.

Configuration (environment variables):

| Variable               | Required | Default | Notes |
|------------------------|----------|---------|-------|
| `S3_ENDPOINT`          | yes      |         | e.g. `https://t3.storageapi.dev` |
| `S3_BUCKET`            | yes      |         | |
| `S3_ACCESS_KEY_ID`     | yes      |         | |
| `S3_SECRET_ACCESS_KEY` | yes      |         | |
| `S3_REGION`            | no       | `auto`  | |
| `S3_USE_PATH_STYLE`    | no       | `false` | `true` for path-style URLs |
| `PORT`                 | no       | `8080`  | Set by Railway |

### Bucket

Railway bucket **`picapi-images`**. Objects are stored at their key; the key is
the URL path (`https://img.janharkonen.fi/<key>`).

## Deployment

Hosted on [Railway](https://railway.com), project **picapi-v2**, environment
`production`.

| Service / resource | What it is | Domain |
|--------------------|------------|--------|
| `picapi`           | Go API, built from `api/Dockerfile` | `img.janharkonen.fi` |
| `picapi-images`    | Railway bucket the API reads from | — |

- The `picapi` service's `S3_*` variables hold the `picapi-images` bucket's
  credentials.
- Railway's CDN caching is disabled for `picapi`.
- The service is not connected to GitHub. Deploy from the repository root with
  `railway up -s picapi`; the build context is the repository root and
  `RAILWAY_DOCKERFILE_PATH=api/Dockerfile` selects the Dockerfile.

## Local development

Run against the real bucket using the service's variables:

```sh
cd api && go build -o /tmp/picapi . && cd ..
PORT=8080 railway run -s picapi /tmp/picapi
curl -i localhost:8080/<key>
```

Or copy `api/.env.example` to `api/.env`, fill in bucket credentials, and
`cd api && set -a && . ./.env && set +a && go run .`
