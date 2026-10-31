// Server-side client for the picapi Go API. It forwards the visitor's session cookie, so
// this app holds no credentials of its own.

import { PICAPI_API_URL } from '$app/env/private';
import { getRequestEvent } from '$app/server';
import { error } from '@sveltejs/kit';
import type { AssetPage } from '#lib/types.ts';

export const SESSION_COOKIE = 'picapi_session';

function apiURL(path: string): string {
	return `${PICAPI_API_URL}/api/v1${path}`;
}

/** Calls the API as the current visitor. Throws a SvelteKit error on failure. */
export async function picapi(path: string, init: RequestInit = {}): Promise<Response> {
	const { cookies } = getRequestEvent();
	const headers = new Headers(init.headers);
	const session = cookies.get(SESSION_COOKIE);
	if (session) headers.set('Cookie', `${SESSION_COOKIE}=${session}`);
	headers.set('X-Picapi-CSRF', '1');
	const res = await fetch(apiURL(path), { ...init, headers });
	if (!res.ok) {
		const body = (await res.json().catch(() => ({}))) as { error?: string };
		error(res.status, body.error ?? res.statusText);
	}
	return res;
}

/** Calls the API without the visitor's session (used to log in). */
export function picapiAnonymous(path: string, init: RequestInit = {}): Promise<Response> {
	return fetch(apiURL(path), init);
}

/**
 * Yields once per asset change event from the API's Server-Sent Events stream, until the
 * signal aborts. Bursts are coalesced: a yield means "something changed, reload".
 */
export async function* assetChanges(signal: AbortSignal): AsyncGenerator<void> {
	const res = await picapi('/events', { signal });
	const reader = res.body!.pipeThrough(new TextDecoderStream()).getReader();
	let buffer = '';
	try {
		while (!signal.aborted) {
			const { value, done } = await reader.read();
			if (done) return;
			buffer += value;
			let changed = false;
			let end: number;
			while ((end = buffer.indexOf('\n\n')) >= 0) {
				const message = buffer.slice(0, end);
				buffer = buffer.slice(end + 2);
				if (message.split('\n').some((line) => line.startsWith('data:'))) changed = true;
			}
			if (changed) yield;
		}
	} finally {
		reader.cancel().catch(() => {});
	}
}
