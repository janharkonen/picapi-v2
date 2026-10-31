<script lang="ts">
	import { login } from '#lib/picapi.remote.ts';
</script>

<form {...login} class="card narrow">
	<h1>picapi</h1>
	<label>
		API key
		<input {...login.fields._key.as('password')} autocomplete="current-password" required />
	</label>
	{#each login.fields._key.issues() ?? [] as issue}
		<p class="error">{issue.message}</p>
	{/each}
	<button type="submit" disabled={!!login.pending}>Log in</button>
</form>
