import { command, form, getRequestEvent, query } from '$app/server';
import { error, invalid } from '@sveltejs/kit';
import * as v from 'valibot';
import { SESSION_COOKIE, assetChanges, picapi, picapiAnonymous } from '#lib/server/picapi.ts';
import type { AssetPage } from '#lib/types.ts';

/** Whether the visitor has a valid session. */
export const getSession = query(async () => {
	const { cookies } = getRequestEvent();
	if (!cookies.get(SESSION_COOKIE)) return { loggedIn: false };
	try {
		await picapi('/session');
		return { loggedIn: true };
	} catch {
		return { loggedIn: false };
	}
});

/** Exchanges the API key for the API's session cookie. */
export const login = form(v.object({ _key: v.pipe(v.string(), v.nonEmpty()) }), async ({ _key }, issue) => {
	const res = await picapiAnonymous('/session', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ key: _key })
	});
	if (!res.ok) invalid(issue._key('Invalid API key'));

	// Re-issue the API's cookie on this response; the browser only talks to this origin.
	const setCookie = res.headers.get('set-cookie') ?? '';
	const match = setCookie.match(new RegExp(`${SESSION_COOKIE}=([^;]+)`));
	const maxAge = Number(setCookie.match(/Max-Age=(\d+)/i)?.[1] ?? 2592000);
	if (!match) error(502, 'API did not return a session');
	const { cookies, url } = getRequestEvent();
	cookies.set(SESSION_COOKIE, match[1], {
		path: '/',
		httpOnly: true,
		sameSite: 'strict',
		secure: url.protocol === 'https:',
		maxAge,
		encode: (value) => value
	});
	void getSession().refresh();
});

export const logout = command(async () => {
	getRequestEvent().cookies.delete(SESSION_COOKIE, { path: '/' });
	void getSession().refresh();
});

/**
 * The newest `limit` assets, live: the list is re-sent to every open page whenever an
 * asset is uploaded or deleted (by the UI, the CLI, MCP or the API).
 */
export const liveAssets = query.live(
	v.pipe(v.number(), v.integer(), v.minValue(1), v.maxValue(500)),
	async function* (limit) {
		const controller = new AbortController();
		const changes = assetChanges(controller.signal);
		try {
			yield await listAssets(limit);
			for await (const _ of changes) {
				yield await listAssets(limit);
			}
		} finally {
			controller.abort();
		}
	}
);

async function listAssets(limit: number): Promise<AssetPage & { total_shown: number }> {
	const items: AssetPage['items'] = [];
	let cursor: string | null = null;
	let more = false;
	do {
		const query = new URLSearchParams({ limit: String(Math.min(100, limit - items.length)) });
		if (cursor) query.set('cursor', cursor);
		const page = (await (await picapi(`/assets?${query}`)).json()) as AssetPage;
		items.push(...page.items);
		cursor = page.next_cursor;
		more = cursor !== null;
	} while (more && items.length < limit);
	return { items, next_cursor: more ? cursor : null, total_shown: items.length };
}

export const deleteAsset = command(v.pipe(v.string(), v.regex(/^[0-9A-Za-z]{12}$/)), async (id) => {
	await picapi(`/assets/${id}`, { method: 'DELETE' });
});

/** Builds a canonical (and, when needed, signed) delivery URL. */
export const buildUrl = command(
	v.object({
		id: v.pipe(v.string(), v.regex(/^[0-9A-Za-z]{12}$/)),
		transforms: v.string(),
		slug: v.string()
	}),
	async ({ id, transforms, slug }) => {
		const body: Record<string, string> = {};
		if (transforms) body.transforms = transforms;
		if (slug) body.slug = slug;
		const res = await picapi(`/assets/${id}/url`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(body)
		});
		return ((await res.json()) as { url: string }).url;
	}
);
