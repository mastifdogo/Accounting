<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import ErrorNotice from '#lib/components/ErrorNotice.svelte';
	import { api, unwrap } from '#lib/api/client.ts';
	import { _safeNext } from '../+layout';

	let username = $state('');
	let password = $state('');
	let busy = $state(false);
	let error = $state<unknown>(null);


	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = null;
		try {
			unwrap(await api().POST('/auth/login', { body: { username, password } }));
			password = '';
			// Re-run the layout load so it picks up the new session.
			await goto(_safeNext(page.url.searchParams.get('next')), { replace: true, refreshAll: true });
		} catch (err) {
			error = err;
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Log in · Ledger</title></svelte:head>

<form class="card login" onsubmit={submit}>
	<h1>Ledger</h1>
	<ErrorNotice {error} />
	<label>Username <input bind:value={username} autocomplete="username" required autocapitalize="none" spellcheck="false" /></label>
	<label>Password <input type="password" bind:value={password} autocomplete="current-password" required /></label>
	<button class="primary" disabled={busy}>{busy ? 'Logging in…' : 'Log in'}</button>
	<p class="muted small">Accounts are created by the administrator with <code>ledger user add</code>.</p>
</form>

<style>
	.login {
		max-width: 360px;
		margin: 12vh auto 0;
		display: flex;
		flex-direction: column;
		gap: 0.9rem;
		padding: 1.5rem;
	}
	.login h1 {
		margin: 0;
	}
	.login p {
		margin: 0;
	}
</style>
