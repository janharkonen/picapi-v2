<script lang="ts">
	import { liveAssets, logout } from '#lib/picapi.remote.ts';
	import type { Asset } from '#lib/types.ts';
	import Detail from './Detail.svelte';

	const PAGE = 48;
	let limit = $state(PAGE);
	// Live: updates by itself whenever an asset is uploaded or deleted anywhere.
	const assets = $derived(liveAssets(limit));

	let selected = $state<Asset | null>(null);
	let status = $state('');
	let dragging = $state(false);

	async function upload(files: FileList | File[]) {
		const list = [...files].filter((f) => f.type.startsWith('image/'));
		for (const [i, file] of list.entries()) {
			status = `Uploading ${i + 1}/${list.length}: ${file.name}`;
			const body = new FormData();
			body.append('file', file);
			// Straight to the Go API (same origin), so large files don't pass through this app.
			const res = await fetch('/api/v1/assets', {
				method: 'POST',
				body,
				headers: { 'X-Picapi-CSRF': '1' }
			});
			if (!res.ok) {
				const err = (await res.json().catch(() => ({}))) as { error?: string };
				status = `${file.name}: ${err.error ?? res.statusText}`;
				return;
			}
		}
		status = '';
	}

	async function signOut() {
		await fetch('/api/v1/session', { method: 'DELETE' });
		await logout();
	}
</script>

<svelte:document
	ondragover={(e) => {
		e.preventDefault();
		dragging = true;
	}}
	ondragleave={() => (dragging = false)}
	ondrop={(e) => {
		e.preventDefault();
		dragging = false;
		if (e.dataTransfer) upload(e.dataTransfer.files);
	}}
/>

<header>
	<h1>picapi</h1>
	<span class="live" class:on={assets.connected}>{assets.connected ? 'live' : 'reconnecting'}</span>
	<label class="button">
		Upload
		<input
			type="file"
			accept="image/jpeg,image/png,image/gif,image/webp,image/avif"
			multiple
			hidden
			onchange={(e) => {
				upload(e.currentTarget.files ?? []);
				e.currentTarget.value = '';
			}}
		/>
	</label>
	<button class="secondary" onclick={signOut}>Log out</button>
</header>

{#if status}<p class="status">{status}</p>{/if}
<div class="drop-hint" class:dragging>Drop images anywhere to upload</div>

<svelte:boundary>
	{@const page = await assets}
	{#if page.items.length === 0}
		<p class="status">No images yet. Upload one to get started.</p>
	{/if}
	<main class="grid">
		{#each page.items as asset (asset.id)}
			<button class="tile" type="button" onclick={() => (selected = asset)}>
				<img loading="lazy" alt={asset.filename} src="/i/{asset.id}/p_thumb" />
				<span>{asset.filename}</span>
				<span class="badge">{asset.width}×{asset.height}</span>
			</button>
		{/each}
	</main>
	{#if page.next_cursor}
		<button id="more" class="secondary" onclick={() => (limit += PAGE)}>Load more</button>
	{/if}

	{#snippet pending()}
		<p class="status">Loading images…</p>
	{/snippet}
</svelte:boundary>

{#if selected}
	<Detail asset={selected} onclose={() => (selected = null)} />
{/if}
