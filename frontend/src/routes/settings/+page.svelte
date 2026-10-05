<script lang="ts">
	import ErrorNotice from '#lib/components/ErrorNotice.svelte';
	import { api, unwrap } from '#lib/api/client.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	let current = $state('');
	let next = $state('');
	let repeat = $state('');
	let busy = $state(false);
	let error = $state<unknown>(null);
	let done = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = null;
		done = false;
		if (next !== repeat) {
			error = new Error('The new passwords do not match.');
			return;
		}
		busy = true;
		try {
			const res = await api().POST('/auth/password', { body: { current_password: current, new_password: next } });
			if (res.response.status !== 204) unwrap(res);
			current = next = repeat = '';
			done = true;
		} catch (err) {
			error = err;
		} finally {
			busy = false;
		}
	}
</script>

<h1>Account settings</h1>

<div class="card">
	<p style="margin-top:0">
		Signed in as <strong>{data.user?.username}</strong>.
		{#if data.user?.last_login_at}<span class="muted small">Last login {new Date(data.user.last_login_at).toLocaleString()}.</span>{/if}
	</p>
</div>

<form class="card" onsubmit={submit} style="max-width:480px">
	<h2 style="margin-top:0">Change password</h2>
	{#if done}<div class="notice ok">Password changed. Your other sessions were logged out.</div>{/if}
	<ErrorNotice {error} />
	<div style="display:flex; flex-direction:column; gap:0.8rem">
		<label>Current password <input type="password" bind:value={current} autocomplete="current-password" required /></label>
		<label>New password <input type="password" bind:value={next} autocomplete="new-password" required minlength="10" /></label>
		<label>Repeat new password <input type="password" bind:value={repeat} autocomplete="new-password" required minlength="10" /></label>
		<div><button class="primary" disabled={busy}>Change password</button></div>
	</div>
	<p class="muted small" style="margin-bottom:0">At least 10 characters. A few random words make a strong, memorable password.</p>
</form>
