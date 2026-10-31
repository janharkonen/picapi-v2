// Black-box behavior every picapi implementation must match.
//   BASE_URL=http://localhost:8081 API_KEY=... node --test conformance.test.ts

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { before, describe, test } from "node:test";
import sharp from "sharp";

const baseUrl = (process.env.BASE_URL ?? "http://localhost:8081").replace(/\/+$/, "");
const apiKey = process.env.API_KEY ?? "dev-api-key-0123456789abcdefghijklmn";
const transformVersion = process.env.TRANSFORM_VERSION ?? "1";

type Asset = {
  id: string;
  filename: string;
  content_type: string;
  size_bytes: number;
  width: number;
  height: number;
  sha256: string;
  visibility: "public" | "private";
  created_at: string;
  url: string;
};

function api(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set("Authorization", `Bearer ${apiKey}`);
  return fetch(`${baseUrl}/api/v1${path}`, { ...init, headers });
}

function postJson(path: string, body: unknown, method = "POST"): Promise<Response> {
  return api(path, { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
}

async function upload(data: Buffer, filename: string, fields: Record<string, string> = {}): Promise<Response> {
  const form = new FormData();
  form.append("file", new Blob([new Uint8Array(data)]), filename);
  for (const [key, value] of Object.entries(fields)) form.append(key, value);
  return api("/assets", { method: "POST", body: form });
}

async function uploadOk(data: Buffer, filename: string, fields: Record<string, string> = {}): Promise<Asset> {
  const res = await upload(data, filename, fields);
  assert.equal(res.status, 201, await res.clone().text());
  return (await res.json()) as Asset;
}

function get(path: string, init: RequestInit = {}): Promise<Response> {
  return fetch(path.startsWith("http") ? path : `${baseUrl}${path}`, { redirect: "manual", ...init });
}

async function imageOf(res: Response) {
  const buf = Buffer.from(await res.arrayBuffer());
  const meta = await sharp(buf).metadata();
  return { buf, format: meta.format, width: meta.width, height: meta.height };
}

async function expectError(res: Response, status: number) {
  assert.equal(res.status, status, await res.clone().text());
  assert.equal(res.headers.get("cache-control"), "no-store");
  const body = (await res.json()) as { error?: unknown };
  assert.equal(typeof body.error, "string");
}

function expectedHash(id: string, r: { w?: number; h?: number; fit?: string; g?: string; f: string; q?: number; up?: boolean }) {
  const input =
    `${transformVersion}|${id}|w=${r.w ?? ""};h=${r.h ?? ""};fit=${r.fit ?? "inside"};g=${r.g ?? "center"};` +
    `f=${r.f};q=${r.q ?? ""};up=${r.up ? 1 : 0};bg=0;bgc=`;
  return createHash("sha256").update(input).digest("hex").slice(0, 32);
}

const solid = (width: number, height: number, format: "jpeg" | "png" = "jpeg", alpha = false) =>
  sharp({ create: { width, height, channels: alpha ? 4 : 3, background: alpha ? "#33669980" : "#336699" } })
    [format]()
    .toBuffer();

let photo: Asset; // 1600x1000 jpeg
let small: Asset; // 400x200 png with alpha

before(async () => {
  photo = await uploadOk(await solid(1600, 1000), "photo.jpg");
  small = await uploadOk(await solid(400, 200, "png", true), "small.png");
});

describe("health and auth", () => {
  test("healthcheck", async () => {
    const res = await get("/healthcheck");
    assert.equal(res.status, 200);
    assert.equal(await res.text(), "ok");
  });

  test("missing and wrong API key are rejected", async () => {
    await expectError(await fetch(`${baseUrl}/api/v1/assets`), 401);
    await expectError(await fetch(`${baseUrl}/api/v1/assets`, { headers: { Authorization: "Bearer nope" } }), 401);
  });
});

describe("upload", () => {
  test("returns the asset", () => {
    assert.match(photo.id, /^[0-9A-Za-z]{12}$/);
    assert.equal(photo.filename, "photo.jpg");
    assert.equal(photo.content_type, "image/jpeg");
    assert.equal(photo.width, 1600);
    assert.equal(photo.height, 1000);
    assert.equal(photo.visibility, "public");
    assert.match(photo.sha256, /^[0-9a-f]{64}$/);
    assert.match(photo.created_at, /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/);
    assert.equal(photo.url, `${baseUrl}/i/${photo.id}`);
  });

  test("reports dimensions after EXIF orientation", async () => {
    const rotated = await sharp({ create: { width: 300, height: 100, channels: 3, background: "#ff0000" } })
      .jpeg()
      .withMetadata({ orientation: 6 })
      .toBuffer();
    const asset = await uploadOk(rotated, "rotated.jpg");
    assert.equal(asset.width, 100);
    assert.equal(asset.height, 300);
    const out = await imageOf(await get(`/i/${asset.id}/w_64`));
    assert.equal(out.width, 64);
    assert.equal(out.height, 192);
  });

  test("accepts visibility and filename fields", async () => {
    const asset = await uploadOk(await solid(10, 10), "x.jpg", { visibility: "private", filename: "dir/name.jpg" });
    assert.equal(asset.visibility, "private");
    assert.equal(asset.filename, "name.jpg");
  });

  test("rejects non-images, empty files and missing file", async () => {
    await expectError(await upload(Buffer.from("hello world, not an image"), "a.txt"), 415);
    await expectError(await upload(Buffer.alloc(0), "empty.jpg"), 400);
    const form = new FormData();
    form.append("other", "x");
    await expectError(await api("/assets", { method: "POST", body: form }), 400);
    await expectError(await upload(Buffer.from([0xff, 0xd8, 0xff, 0x00, 0x01]), "broken.jpg"), 400);
    await expectError(await upload(await solid(10, 10), "x.jpg", { visibility: "secret" }), 400);
  });
});

describe("asset management", () => {
  test("get and 404s", async () => {
    const res = await api(`/assets/${photo.id}`);
    assert.equal(res.status, 200);
    assert.deepEqual(await res.json(), photo);
    await expectError(await api("/assets/AAAAAAAAAAAA"), 404);
    await expectError(await api("/assets/not-an-id"), 404);
  });

  test("list paginates newest first", async () => {
    const a = await uploadOk(await solid(8, 8), "a.jpg");
    const b = await uploadOk(await solid(8, 8), "b.jpg");
    const first = (await (await api("/assets?limit=1")).json()) as { items: Asset[]; next_cursor: string | null };
    assert.equal(first.items.length, 1);
    assert.equal(first.items[0]!.id, b.id);
    assert.equal(first.next_cursor, b.id);
    const second = (await (await api(`/assets?limit=1&cursor=${first.next_cursor}`)).json()) as { items: Asset[] };
    assert.equal(second.items[0]!.id, a.id);

    const all = (await (await api("/assets?limit=100")).json()) as { items: Asset[]; next_cursor: string | null };
    assert.ok(all.items.some((item) => item.id === photo.id));

    await expectError(await api("/assets?limit=0"), 400);
    await expectError(await api("/assets?limit=101"), 400);
    await expectError(await api("/assets?limit=abc"), 400);
    await expectError(await api("/assets?cursor=AAAAAAAAAAAA"), 400);
  });

  test("patch filename and visibility", async () => {
    const asset = await uploadOk(await solid(8, 8), "before.jpg");
    const res = await postJson(`/assets/${asset.id}`, { filename: "after.jpg", visibility: "private" }, "PATCH");
    assert.equal(res.status, 200);
    const updated = (await res.json()) as Asset;
    assert.equal(updated.filename, "after.jpg");
    assert.equal(updated.visibility, "private");
    await expectError(await postJson(`/assets/${asset.id}`, {}, "PATCH"), 400);
    await expectError(await postJson(`/assets/${asset.id}`, { visibility: "hidden" }, "PATCH"), 400);
    await expectError(await postJson(`/assets/${asset.id}`, { filename: 5 }, "PATCH"), 400);
    await expectError(await api(`/assets/${asset.id}`, { method: "PATCH", body: "{oops" }), 400);
  });

  test("delete", async () => {
    const asset = await uploadOk(await solid(8, 8), "gone.jpg");
    assert.equal((await get(`/i/${asset.id}/w_64`)).status, 200);
    const res = await api(`/assets/${asset.id}`, { method: "DELETE" });
    assert.equal(res.status, 204);
    await expectError(await api(`/assets/${asset.id}`), 404);
    await expectError(await get(`/i/${asset.id}`), 404);
    await expectError(await api(`/assets/${asset.id}`, { method: "DELETE" }), 404);
  });
});

describe("delivery: originals", () => {
  test("serves the original bytes with immutable caching", async () => {
    const res = await get(`/i/${photo.id}`);
    assert.equal(res.status, 200);
    assert.equal(res.headers.get("content-type"), "image/jpeg");
    assert.equal(res.headers.get("cache-control"), "public, max-age=31536000, immutable");
    assert.equal(res.headers.get("etag"), `"${photo.sha256}"`);
    assert.equal(res.headers.get("x-content-type-options"), "nosniff");
    const buf = Buffer.from(await res.arrayBuffer());
    assert.equal(createHash("sha256").update(buf).digest("hex"), photo.sha256);
  });

  test("conditional requests return 304", async () => {
    const res = await get(`/i/${photo.id}`, { headers: { "If-None-Match": `"${photo.sha256}"` } });
    assert.equal(res.status, 304);
    assert.equal(res.headers.get("etag"), `"${photo.sha256}"`);
  });

  test("HEAD works", async () => {
    const res = await get(`/i/${photo.id}/w_320`, { method: "HEAD" });
    assert.equal(res.status, 200);
    assert.equal(res.headers.get("content-type"), "image/jpeg");
  });

  test("unknown and malformed IDs are 404", async () => {
    await expectError(await get("/i/AAAAAAAAAAAA"), 404);
    await expectError(await get("/i/short"), 404);
  });

  test("slugs", async () => {
    assert.equal((await get(`/i/${photo.id}/team-photo.jpg`)).status, 200);
    const res = await get(`/i/${photo.id}/w_320,f_webp/team-photo.webp`);
    assert.equal(res.status, 200);
    assert.equal(res.headers.get("content-type"), "image/webp");
    await expectError(await get(`/i/${photo.id}/w_320/no-extension`), 400);
    await expectError(await get(`/i/${photo.id}/a.b/c.jpg`), 400);
  });
});

describe("delivery: transforms", () => {
  test("resize and convert", async () => {
    const res = await get(`/i/${photo.id}/w_320,f_webp`);
    assert.equal(res.status, 200);
    assert.equal(res.headers.get("content-type"), "image/webp");
    assert.equal(res.headers.get("cache-control"), "public, max-age=31536000, immutable");
    const hash = expectedHash(photo.id, { w: 320, f: "webp", q: 80 });
    assert.equal(res.headers.get("x-picapi-variant"), hash);
    assert.equal(res.headers.get("etag"), `"${hash}"`);
    const out = await imageOf(res);
    assert.deepEqual([out.format, out.width, out.height], ["webp", 320, 200]);
  });

  test("second request is served from the bucket cache", async () => {
    const path = `/i/${photo.id}/w_480,f_avif,q_60`;
    assert.equal((await get(path)).headers.get("x-picapi-cache"), "miss");
    let cache: string | null = null;
    for (let i = 0; i < 20 && cache !== "hit"; i++) {
      await new Promise((resolve) => setTimeout(resolve, 100));
      const res = await get(path);
      cache = res.headers.get("x-picapi-cache");
      await res.arrayBuffer();
    }
    assert.equal(cache, "hit");
    const out = await imageOf(await get(path));
    assert.deepEqual([out.format, out.width, out.height], ["heif", 480, 300]);
  });

  test("variant 304", async () => {
    const hash = expectedHash(photo.id, { w: 320, f: "webp", q: 80 });
    const res = await get(`/i/${photo.id}/w_320,f_webp`, { headers: { "If-None-Match": `"${hash}"` } });
    assert.equal(res.status, 304);
  });

  test("default output format and quality follow the original", async () => {
    const res = await get(`/i/${photo.id}/w_320`);
    assert.equal(res.headers.get("x-picapi-variant"), expectedHash(photo.id, { w: 320, f: "jpeg", q: 80 }));
    assert.equal((await imageOf(res)).format, "jpeg");
    const png = await get(`/i/${small.id}/w_64`);
    assert.equal(png.headers.get("x-picapi-variant"), expectedHash(small.id, { w: 64, f: "png" }));
  });

  test("fit modes and upscaling", async () => {
    const dims = async (path: string) => {
      const res = await get(path);
      assert.equal(res.status, 200, `${path}: ${await res.clone().text()}`);
      const out = await imageOf(res);
      return [out.width, out.height];
    };
    assert.deepEqual(await dims(`/i/${small.id}/w_640`), [400, 200]); // never upscales by default
    assert.deepEqual(await dims(`/i/${small.id}/w_640,up_1`), [640, 320]);
    assert.deepEqual(await dims(`/i/${small.id}/h_128`), [256, 128]);
    assert.deepEqual(await dims(`/i/${small.id}/w_128,h_128`), [128, 64]);
    assert.deepEqual(await dims(`/i/${small.id}/w_128,h_128,fit_cover`), [128, 128]);
    assert.deepEqual(await dims(`/i/${small.id}/w_128,h_128,fit_cover,g_e`), [128, 128]);
    assert.deepEqual(await dims(`/i/${small.id}/w_256,h_256,fit_cover,g_attention`), [256, 200]); // cropped, not upscaled
    assert.deepEqual(await dims(`/i/${small.id}/w_256,h_128,fit_fill`), [256, 128]);
    assert.deepEqual(await dims(`/i/${small.id}/w_640,h_640,fit_fill`), [400, 200]);
    assert.deepEqual(await dims(`/i/${photo.id}/w_320,dpr_2`), [640, 400]);
  });

  test("jpeg output flattens alpha", async () => {
    const out = await imageOf(await get(`/i/${small.id}/f_jpeg`));
    assert.equal(out.format, "jpeg");
    const { data } = await sharp(out.buf).raw().toBuffer({ resolveWithObject: true });
    // #336699 at 50% alpha over white
    assert.ok(Math.abs(data[0]! - 0x99) < 6 && Math.abs(data[2]! - 0xcc) < 6, `got ${data[0]},${data[1]},${data[2]}`);
  });

  test("presets", async () => {
    let res = await get(`/i/${photo.id}/p_thumb`);
    assert.equal(res.headers.get("x-picapi-variant"), expectedHash(photo.id, { w: 128, h: 128, fit: "cover", g: "attention", f: "webp", q: 70 }));
    let out = await imageOf(res);
    assert.deepEqual([out.format, out.width, out.height], ["webp", 128, 128]);

    res = await get(`/i/${photo.id}/p_thumb,f_avif`);
    out = await imageOf(res);
    assert.deepEqual([out.format, out.width, out.height], ["heif", 128, 128]);

    out = await imageOf(await get(`/i/${photo.id}/p_og`));
    assert.deepEqual([out.format, out.width, out.height], ["jpeg", 1200, 630]); // presets may leave the grid
    out = await imageOf(await get(`/i/${photo.id}/p_small,fit_inside`));
    assert.deepEqual([out.width, out.height], [320, 200]);
    await expectError(await get(`/i/${photo.id}/p_huge`), 400);
  });

  test("invalid transforms are 400", async () => {
    for (const t of [
      "x_1",
      "w_320,w_640",
      "w_0",
      "w_0320",
      "w_5000",
      "w",
      "w_",
      "_320",
      "fit_cover,w_320",
      "fit_fill,h_320",
      "g_n",
      "w_320,g_attention",
      "f_png,q_80",
      "f_gif",
      "f_webp,q_0",
      "f_webp,q_101",
      "dpr_4",
      "up_2",
      "w_1920,dpr_3",
      "bg_keep",
      "bgc_FFFFFF",
      "bgc_ffffff",
      "bg_remove,f_jpeg",
    ]) {
      const res = await get(`/i/${photo.id}/${t}`);
      assert.equal(res.status, 400, `${t}: expected 400, got ${res.status} ${await res.clone().text()}`);
      await expectError(res, 400);
    }
  });
});

describe("delivery: canonical URLs", () => {
  const redirects = (from: string, to: string) => async () => {
    const res = await get(from);
    assert.equal(res.status, 301, await res.clone().text());
    assert.equal(res.headers.get("location"), to);
    assert.equal(res.headers.get("cache-control"), "public, max-age=31536000");
  };

  test("query form", () => redirects(`/i/${photo.id}?f=webp&w=320`, `/i/${photo.id}/w_320,f_webp`)());
  test("key order", () => redirects(`/i/${photo.id}/f_webp,w_320`, `/i/${photo.id}/w_320,f_webp`)());
  test("query form with slug", () => redirects(`/i/${photo.id}/a.webp?w=320`, `/i/${photo.id}/w_320/a.webp`)());
  test("unknown query parameters are dropped", () => redirects(`/i/${photo.id}/w_320?v=2`, `/i/${photo.id}/w_320`)());

  test("transforms in both path and query are 400", async () => {
    await expectError(await get(`/i/${photo.id}/w_320?f=webp`), 400);
  });
});

describe("signed URLs", () => {
  test("off-grid values need a signature", async () => {
    await expectError(await get(`/i/${photo.id}/w_300`), 403);
    await expectError(await get(`/i/${photo.id}/w_320,q_75`), 403);
  });

  test("the url endpoint builds canonical, signed URLs", async () => {
    let res = await postJson(`/assets/${photo.id}/url`, { transforms: "f_webp,w_300" });
    assert.equal(res.status, 200);
    const { url } = (await res.json()) as { url: string };
    assert.match(url, new RegExp(`^${baseUrl}/i/${photo.id}/w_300,f_webp\\?s=[A-Za-z0-9_-]{43}$`));
    const out = await imageOf(await get(url));
    assert.equal(out.width, 300);

    const tampered = url.replace("w_300", "w_301");
    await expectError(await get(tampered), 403);
    await expectError(await get(url.replace(/s=.{5}/, "s=AAAAA")), 403);

    res = await postJson(`/assets/${photo.id}/url`, { transforms: "w_320", slug: "nice.jpg" });
    assert.equal(((await res.json()) as { url: string }).url, `${baseUrl}/i/${photo.id}/w_320/nice.jpg`);

    res = await postJson(`/assets/${photo.id}/url`, {});
    assert.equal(((await res.json()) as { url: string }).url, `${baseUrl}/i/${photo.id}`);

    await expectError(await postJson(`/assets/${photo.id}/url`, { transforms: "w_99999" }), 400);
    await expectError(await postJson(`/assets/${photo.id}/url`, { transforms: 5 }), 400);
    await expectError(await postJson(`/assets/${photo.id}/url`, { slug: "no extension" }), 400);
  });

  test("expiring URLs", async () => {
    const res = await postJson(`/assets/${photo.id}/url`, { transforms: "w_320", expires_in: 1 });
    const { url } = (await res.json()) as { url: string };
    assert.match(url, /\?exp=\d+&s=/);
    assert.equal((await get(url)).status, 200);
    await new Promise((resolve) => setTimeout(resolve, 2100));
    await expectError(await get(url), 403);
    await expectError(await postJson(`/assets/${photo.id}/url`, { expires_in: 0 }), 400);
  });

  test("background removal requires a signature", async () => {
    await expectError(await get(`/i/${photo.id}/f_png,bg_remove`), 403);
  });
});

describe("background removal", () => {
  let circle: Asset;
  before(async () => {
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="600" height="400">
      <rect width="600" height="400" fill="#ebebe1"/><circle cx="300" cy="200" r="120" fill="#c82828"/></svg>`;
    circle = await uploadOk(await sharp(Buffer.from(svg)).png().toBuffer(), "circle.png");
  });

  const signedUrl = async (transforms: string) => {
    const res = await postJson(`/assets/${circle.id}/url`, { transforms });
    assert.equal(res.status, 200, await res.clone().text());
    return ((await res.json()) as { url: string }).url;
  };

  test("cuts out the subject with transparency", async () => {
    const res = await get(await signedUrl("f_png,bg_remove"));
    assert.equal(res.status, 200, await res.clone().text());
    const { data, info } = await sharp(Buffer.from(await res.arrayBuffer())).raw().toBuffer({ resolveWithObject: true });
    assert.equal(info.channels, 4);
    const alpha = (x: number, y: number) => data[(y * info.width + x) * 4 + 3]!;
    assert.ok(alpha(5, 5) < 30, `corner alpha ${alpha(5, 5)}`);
    assert.ok(alpha(300, 200) > 220, `center alpha ${alpha(300, 200)}`);
  });

  test("combines with resizing, other formats and a background color", async () => {
    let out = await imageOf(await get(await signedUrl("w_256,f_webp,bg_remove")));
    assert.deepEqual([out.format, out.width, out.height], ["webp", 256, 171]);
    const res = await get(await signedUrl("w_128,f_jpeg,bg_remove,bgc_0000ff"));
    out = await imageOf(res);
    assert.deepEqual([out.format, out.width], ["jpeg", 128]);
    const { data } = await sharp(out.buf).raw().toBuffer({ resolveWithObject: true });
    assert.ok(data[2]! > 200 && data[0]! < 60, `corner should be blue, got ${data[0]},${data[1]},${data[2]}`);
  });
});

describe("API keys", () => {
  test("create, use and revoke", async () => {
    const list = (await (await api("/keys")).json()) as { items: { id: string; name: string; revoked_at: string | null }[] };
    assert.ok(list.items.length >= 1);

    const res = await postJson("/keys", { name: "conformance" });
    assert.equal(res.status, 201);
    const created = (await res.json()) as { id: string; name: string; key: string };
    assert.match(created.key, /^pk_[0-9A-Za-z]{40}$/);
    assert.equal(created.name, "conformance");

    const withNewKey = await fetch(`${baseUrl}/api/v1/assets?limit=1`, { headers: { Authorization: `Bearer ${created.key}` } });
    assert.equal(withNewKey.status, 200);

    const keys = (await (await api("/keys")).json()) as { items: { id: string; key?: string }[] };
    assert.ok(keys.items.every((k) => k.key === undefined), "keys are never listed");

    assert.equal((await api(`/keys/${created.id}`, { method: "DELETE" })).status, 204);
    await expectError(await fetch(`${baseUrl}/api/v1/assets`, { headers: { Authorization: `Bearer ${created.key}` } }), 401);
    await expectError(await api(`/keys/${created.id}`, { method: "DELETE" }), 404);
    await expectError(await postJson("/keys", { name: "" }), 400);
  });
});

describe("web UI session", () => {
  test("login with a key gives a cookie session", async () => {
    await expectError(await fetch(`${baseUrl}/api/v1/session`, { method: "POST", body: JSON.stringify({ key: "nope" }) }), 401);
    const login = await fetch(`${baseUrl}/api/v1/session`, { method: "POST", body: JSON.stringify({ key: apiKey }) });
    assert.equal(login.status, 200);
    const setCookie = login.headers.get("set-cookie")!;
    assert.match(setCookie, /picapi_session=[^;]+/);
    assert.match(setCookie, /HttpOnly/i);
    assert.match(setCookie, /SameSite=Strict/i);
    const cookie = setCookie.split(";")[0]!;

    const info = await fetch(`${baseUrl}/api/v1/session`, { headers: { Cookie: cookie } });
    assert.equal(info.status, 200);
    assert.equal(((await info.json()) as { auth: string }).auth, "session");

    // Writes with a cookie need the CSRF header.
    const asset = await uploadOk(await solid(20, 20), "session.jpg", { visibility: "private" });
    const patch = (headers: Record<string, string>) =>
      fetch(`${baseUrl}/api/v1/assets/${asset.id}`, {
        method: "PATCH",
        headers: { Cookie: cookie, "Content-Type": "application/json", ...headers },
        body: JSON.stringify({ filename: "renamed.jpg" }),
      });
    await expectError(await patch({}), 403);
    assert.equal((await patch({ "X-Picapi-CSRF": "1" })).status, 200);

    // The UI can show private assets with its cookie.
    const priv = await get(`/i/${asset.id}/w_64`, { headers: { Cookie: cookie } });
    assert.equal(priv.status, 200);
    assert.equal(priv.headers.get("cache-control"), "private, max-age=300");
    await expectError(await get(`/i/${asset.id}/w_64`, { headers: { Cookie: "picapi_session=forged.1.x" } }), 403);

    const logout = await fetch(`${baseUrl}/api/v1/session`, { method: "DELETE", headers: { Cookie: cookie } });
    assert.equal(logout.status, 204);
    assert.match(logout.headers.get("set-cookie")!, /Max-Age=0/i);
  });

  test("UI is served with a strict CSP", async () => {
    const res = await get("/");
    assert.equal(res.status, 200);
    assert.match(res.headers.get("content-type")!, /text\/html/);
    assert.match(res.headers.get("content-security-policy")!, /default-src 'self'/);
    assert.match(await res.text(), /<title>picapi<\/title>/);
    assert.equal((await get("/app.js")).status, 200);
  });
});

describe("reconcile", () => {
  test("reports bucket and database consistency", async () => {
    const res = await api("/admin/reconcile", { method: "POST" });
    assert.equal(res.status, 200);
    const report = (await res.json()) as Record<string, unknown>;
    assert.equal(report.fixed, false);
    for (const key of ["orphan_originals", "missing_originals", "orphan_variants", "stale_variant_rows"]) {
      assert.ok(Array.isArray(report[key]), key);
    }
    assert.deepEqual(report.missing_originals, []);
  });
});

describe("MCP", () => {
  const rpc = async (method: string, params: unknown = {}, key = apiKey) => {
    const res = await fetch(`${baseUrl}/mcp`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${key}`,
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        "Mcp-Protocol-Version": "2025-06-18",
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    });
    return res;
  };

  test("requires an API key", async () => {
    await expectError(await rpc("tools/list", {}, "nope"), 401);
  });

  test("lists and calls tools", async () => {
    const init = await rpc("initialize", {
      protocolVersion: "2025-06-18",
      capabilities: {},
      clientInfo: { name: "conformance", version: "1" },
    });
    assert.equal(init.status, 200, await init.clone().text());

    const list = (await (await rpc("tools/list")).json()) as { result: { tools: { name: string }[] } };
    const names = list.result.tools.map((t) => t.name).sort();
    for (const name of ["delete_asset", "get_asset", "get_url", "list_assets", "remove_background", "share_asset", "update_asset", "upload_data", "upload_from_url"]) {
      assert.ok(names.includes(name), `missing tool ${name}`);
    }

    const call = async (name: string, args: unknown) => {
      const res = await rpc("tools/call", { name, arguments: args });
      assert.equal(res.status, 200);
      return ((await res.json()) as { result: { isError?: boolean; structuredContent?: any; content: { text: string }[] } }).result;
    };

    const got = await call("get_asset", { id: photo.id });
    assert.equal(got.structuredContent.id, photo.id);

    const url = await call("get_url", { id: photo.id, transforms: "w_300" });
    assert.match(url.structuredContent.url, /\/w_300\?s=/);

    const data = (await solid(30, 20)).toString("base64");
    const uploaded = await call("upload_data", { data, filename: "mcp.jpg" });
    assert.equal(uploaded.structuredContent.width, 30);

    const blocked = await call("upload_from_url", { url: "http://127.0.0.1:8080/healthcheck" });
    assert.equal(blocked.isError, true);
    assert.match(blocked.content[0]!.text, /non-public address/);

    const missing = await call("get_asset", { id: "AAAAAAAAAAAA" });
    assert.equal(missing.isError, true);
  });
});

describe("private assets", () => {
  test("need a valid token", async () => {
    const asset = await uploadOk(await solid(200, 100), "private.jpg", { visibility: "private" });
    await expectError(await get(`/i/${asset.id}`), 403);
    await expectError(await get(`/i/${asset.id}?token=garbage`), 403);

    const res = await postJson(`/assets/${asset.id}/share`, { expires_in: 600, transforms: "w_64" });
    assert.equal(res.status, 200);
    const share = (await res.json()) as { url: string; token: string; expires_at: string };
    assert.equal(share.url, `${baseUrl}/i/${asset.id}/w_64?token=${share.token}`);
    assert.match(share.expires_at, /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/);

    const ok = await get(share.url);
    assert.equal(ok.status, 200);
    assert.match(ok.headers.get("cache-control")!, /^private, max-age=(59\d|600)$/);
    assert.equal(ok.headers.get("referrer-policy"), "no-referrer");
    assert.equal((await imageOf(ok)).width, 64);

    // Tokens are bound to one asset.
    const other = await uploadOk(await solid(10, 10), "other.jpg", { visibility: "private" });
    await expectError(await get(`/i/${other.id}?token=${share.token}`), 403);

    // Redirects of private URLs are not cached.
    const redirect = await get(`/i/${asset.id}?w=64&token=${share.token}`);
    assert.equal(redirect.status, 301);
    assert.equal(redirect.headers.get("cache-control"), "no-store");
    assert.equal(redirect.headers.get("location"), `/i/${asset.id}/w_64?token=${share.token}`);

    await expectError(await postJson(`/assets/${asset.id}/share`, { expires_in: 30 * 24 * 3600 + 1 }), 400);
  });
});
