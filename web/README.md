# picapi web

SvelteKit frontend for picapi. It runs as its own service and is never exposed directly:
the Go API redirects `/` to `/ui` and reverse-proxies `/ui/*` here, so the app shares the
API's origin and session cookie.

- Data comes from [remote functions](https://svelte.dev/docs/kit/remote-functions) in
  `src/lib/picapi.remote.ts`, which call the Go API server-side with the visitor's cookie.
- The image grid is a `query.live`: it streams a fresh list whenever the API's
  `/api/v1/events` stream reports an upload or delete, from any client.
- Uploads go straight from the browser to `POST /api/v1/assets`.

```
cp .env.example .env     # PICAPI_API_URL=http://localhost:8080
npm install
npm run dev              # then open http://localhost:8080 (the Go API)
npm run check
```

Production: `web/Dockerfile` (build context: repository root), `node build` with
`PICAPI_API_URL`, `ORIGIN` (the public URL), `PORT` and `HOST=::`.
