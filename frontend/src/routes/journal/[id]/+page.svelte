<script lang="ts">
	import { goto } from '$app/navigation';
	import Amount from '#lib/components/Amount.svelte';
	import ErrorNotice from '#lib/components/ErrorNotice.svelte';
	import { api, today, unwrap } from '#lib/api/client.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();
	let e = $derived(data.entry);

	let reversing = $state(false);
	let reverseDate = $state(today());
	let busy = $state(false);
	let error = $state<unknown>(null);

	async function reverse(ev: SubmitEvent) {
		ev.preventDefault();
		if (!confirm(`Post a reversing entry for #${e.id} dated ${reverseDate}?`)) return;
		busy = true;
		error = null;
		try {
			const rev = unwrap(
				await api().POST('/journal-entries/{entryId}/reverse', {
					params: { path: { entryId: e.id } },
					body: { entry_date: reverseDate }
				})
			);
			reversing = false;
			await goto(`/journal/${rev.id}`);
		} catch (err) {
			error = err;
		} finally {
			busy = false;
		}
	}
</script>

<p class="small"><a href="/journal">← Journal</a></p>

<div class="toolbar">
	<h1 style="margin:0">Entry #{e.id}</h1>
	{#if e.reverses_id}<span class="badge">reversal of <a href="/journal/{e.reverses_id}">#{e.reverses_id}</a></span>{/if}
	{#if e.reversed_by_id}<span class="badge warn">reversed by <a href="/journal/{e.reversed_by_id}">#{e.reversed_by_id}</a></span>{/if}
	{#if e.import_id}<span class="badge">imported (batch {e.import_id})</span>{/if}
	<span class="spacer"></span>
	{#if !e.reverses_id && !e.reversed_by_id}
		<button class="danger" onclick={() => (reversing = !reversing)}>Reverse…</button>
	{/if}
</div>

<ErrorNotice {error} />

{#if reversing}
	<form class="card toolbar" onsubmit={reverse}>
		<span>Posts a new entry that swaps every debit and credit of #{e.id}.</span>
		<label>Reversal date <input type="date" bind:value={reverseDate} required /></label>
		<button class="danger" disabled={busy}>Post reversal</button>
		<button type="button" onclick={() => (reversing = false)}>Cancel</button>
	</form>
{/if}

<div class="card">
	<div class="grid">
		<div><div class="muted small">Date</div><div class="mono">{e.entry_date}</div></div>
		<div><div class="muted small">Description</div><div>{e.description}</div></div>
		<div><div class="muted small">Reference</div><div>{e.reference || '—'}</div></div>
		<div><div class="muted small">Posted</div><div class="small">{new Date(e.posted_at).toLocaleString()}</div></div>
	</div>
</div>

<div class="table-wrap">
	<table>
		<thead>
			<tr><th>#</th><th>Account</th><th>Memo</th><th>Currency</th><th class="num">Debit</th><th class="num">Credit</th></tr>
		</thead>
		<tbody>
			{#each e.lines as l (l.id)}
				<tr>
					<td class="muted">{l.line_number}</td>
					<td><a class="mono" href="/accounts/{encodeURIComponent(l.account_code)}">{l.account_code}</a> {l.account_name}</td>
					<td>{l.memo}</td>
					<td>{l.currency}</td>
					<td class="num"><Amount cents={l.debit_cents} blankZero /></td>
					<td class="num"><Amount cents={l.credit_cents} blankZero /></td>
				</tr>
			{/each}
		</tbody>
		<tfoot>
			{#each e.totals as t (t.currency)}
				<tr>
					<td colspan="3">Total</td>
					<td>{t.currency}</td>
					<td class="num"><Amount cents={t.amount_cents} /></td>
					<td class="num"><Amount cents={t.amount_cents} /></td>
				</tr>
			{/each}
		</tfoot>
	</table>
</div>
