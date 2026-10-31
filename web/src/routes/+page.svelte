<script lang="ts">
	import { getSession } from '#lib/picapi.remote.ts';
	import Library from './Library.svelte';
	import Login from './Login.svelte';
</script>

<svelte:boundary>
	{#if (await getSession()).loggedIn}
		<Library />
	{:else}
		<Login />
	{/if}

	{#snippet pending()}
		<p class="status">Loading…</p>
	{/snippet}
	{#snippet failed(error, reset)}
		<p class="status error">{(error as Error)?.message ?? 'Something went wrong'}</p>
		<button class="secondary" onclick={reset}>Retry</button>
	{/snippet}
</svelte:boundary>
