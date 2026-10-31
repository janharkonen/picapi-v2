<script lang="ts">
	import { buildUrl, deleteAsset } from '#lib/picapi.remote.ts';
	import type { Asset } from '#lib/types.ts';

	let { asset, onclose }: { asset: Asset; onclose: () => void } = $props();

	let dialog: HTMLDialogElement;
	let preset = $state('');
	let w = $state<number | null>(null);
	let h = $state<number | null>(null);
	let fit = $state('');
	let g = $state('');
	let f = $state('');
	let q = $state<number | null>(null);
	let bg = $state(false);
	let slug = $state('');

	let url = $state('');
	let buildError = $state('');
	let copied = $state(false);

	const transforms = $derived(
		[
			preset && `p_${preset}`,
			w && `w_${w}`,
			h && `h_${h}`,
			fit && `fit_${fit}`,
			g && `g_${g}`,
			f && `f_${f}`,
			q && `q_${q}`,
			bg && 'bg_remove'
		]
			.filter(Boolean)
			.join(',')
	);

	// Rebuild the URL (debounced) whenever the builder changes.
	$effect(() => {
		const args = { id: asset.id, transforms, slug: slug.trim() };
		const timer = setTimeout(async () => {
			try {
				url = await buildUrl(args);
				buildError = '';
			} catch (err) {
				buildError = (err as { body?: { message?: string } })?.body?.message ?? String(err);
			}
		}, 250);
		return () => clearTimeout(timer);
	});

	const preview = $derived.by(() => {
		if (!url) return `/i/${asset.id}`;
		const u = new URL(url);
		return u.pathname + u.search;
	});

	$effect(() => {
		dialog.showModal();
	});

	async function copy() {
		await navigator.clipboard.writeText(url);
		copied = true;
		setTimeout(() => (copied = false), 1200);
	}

	async function remove() {
		if (!confirm(`Delete ${asset.filename}? This cannot be undone.`)) return;
		await deleteAsset(asset.id);
		dialog.close();
	}
</script>

<dialog bind:this={dialog} onclose={onclose}>
	<form method="dialog" class="detail">
		<div class="preview"><img src={preview} alt={asset.filename} /></div>
		<div class="fields">
			<h2>{asset.filename}</h2>
			<p class="meta">
				{asset.id} · {asset.content_type} · {asset.width}×{asset.height} ·
				{Math.round(asset.size_bytes / 1024)} KB · {asset.created_at}
			</p>

			<fieldset>
				<legend>URL builder</legend>
				<div class="row">
					<label>
						Preset
						<select bind:value={preset}>
							<option value="">none</option>
							{#each ['thumb', 'small', 'medium', 'large', 'og'] as p}<option>{p}</option>{/each}
						</select>
					</label>
					<label>Width <input type="number" min="1" max="4096" bind:value={w} /></label>
					<label>Height <input type="number" min="1" max="4096" bind:value={h} /></label>
				</div>
				<div class="row">
					<label>
						Fit
						<select bind:value={fit}>
							<option value="">inside</option><option>cover</option><option>fill</option>
						</select>
					</label>
					<label>
						Gravity
						<select bind:value={g}>
							<option value="">center</option>
							{#each ['attention', 'n', 's', 'e', 'w'] as v}<option>{v}</option>{/each}
						</select>
					</label>
					<label>
						Format
						<select bind:value={f}>
							<option value="">original</option>
							{#each ['webp', 'avif', 'jpeg', 'png'] as v}<option>{v}</option>{/each}
						</select>
					</label>
					<label>Quality <input type="number" min="1" max="100" bind:value={q} /></label>
				</div>
				<div class="row">
					<label class="check"><input type="checkbox" bind:checked={bg} /> Remove background</label>
					<label>Slug <input placeholder="photo.webp" bind:value={slug} /></label>
				</div>
				<div class="url-row">
					<input readonly value={url} />
					<button type="button" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button>
				</div>
				{#if buildError}<p class="error">{buildError}</p>{/if}
			</fieldset>

			<div class="actions">
				<button type="button" class="danger" onclick={remove}>Delete</button>
				<button value="close" class="secondary">Close</button>
			</div>
		</div>
	</form>
</dialog>
