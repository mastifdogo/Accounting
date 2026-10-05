<script lang="ts">
	import { goto, invalidateAll } from '$app/navigation';
	import Amount from '#lib/components/Amount.svelte';
	import ErrorNotice from '#lib/components/ErrorNotice.svelte';
	import { api, unwrap } from '#lib/api/client.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();
	let account = $derived(data.ledger.account);

	let from = $state('');
	let to = $state('');
	$effect.pre(() => {
		from = data.from;
		to = data.to;
	});

	let editing = $state(false);
	let name = $state('');
	let description = $state('');
	let busy = $state(false);
	let error = $state<unknown>(null);

	function applyFilter(e: SubmitEvent) {
		e.preventDefault();
		const q = new URLSearchParams();
		if (from) q.set('from', from);
		if (to) q.set('to', to);
		goto(`?${q}`, { reset: false });
	}

	function startEdit() {
		name = account.name;
		description = account.description;
		editing = true;
	}

	async function update(body: { name?: string; description?: string; is_active?: boolean }) {
		busy = true;
		error = null;
		try {
			unwrap(await api().PATCH('/accounts/{accountCode}', { params: { path: { accountCode: account.code } }, body }));
			editing = false;
			await invalidateAll();
		} catch (err) {
			error = err;
		} finally {
			busy = false;
		}
	}
</script>

<p class="small"><a href="/accounts">← Accounts</a></p>

<div class="toolbar">
	<h1 style="margin:0">
		<span class="mono">{account.code}</span>
		{account.name}
		<span class="badge">{account.type}</span>
		<span class="badge">{account.currency}</span>
		{#if !account.is_active}<span class="badge warn">inactive</span>{/if}
	</h1>
	<span class="spacer"></span>
	<button onclick={startEdit} disabled={busy}>Edit</button>
	<button onclick={() => update({ is_active: !account.is_active })} disabled={busy}>
		{account.is_active ? 'Deactivate' : 'Reactivate'}
	</button>
</div>
{#if account.description && !editing}<p class="muted">{account.description}</p>{/if}

<ErrorNotice {error} />

{#if editing}
	<form
		class="card toolbar"
		onsubmit={(e) => {
			e.preventDefault();
			update({ name, description });
		}}
	>
		<label style="flex:1">Name <input bind:value={name} required maxlength="200" /></label>
		<label style="flex:2">Description <input bind:value={description} maxlength="1000" /></label>
		<button class="primary" disabled={busy}>Save</button>
		<button type="button" onclick={() => (editing = false)}>Cancel</button>
	</form>
{/if}

<form class="toolbar" onsubmit={applyFilter}>
	<label>From <input type="date" bind:value={from} /></label>
	<label>To <input type="date" bind:value={to} /></label>
	<button>Apply</button>
	{#if data.from || data.to}<a class="button" href="?">Clear</a>{/if}
	<span class="spacer"></span>
	<a class="button" href="/journal?account_code={encodeURIComponent(account.code)}">Entries using this account</a>
</form>

<div class="table-wrap">
	<table>
		<thead>
			<tr>
				<th>Date</th><th>Entry</th><th>Description</th>
				<th class="num">Debit</th><th class="num">Credit</th><th class="num">Balance (Dr +)</th>
			</tr>
		</thead>
		<tbody>
			{#if data.from}
				<tr>
					<td class="mono">{data.from}</td><td></td><td class="muted">Opening balance</td><td></td><td></td>
					<td class="num"><Amount cents={data.ledger.opening_balance_cents} /></td>
				</tr>
			{/if}
			{#each data.ledger.lines as l (l.line_id)}
				<tr>
					<td class="mono">{l.entry_date}</td>
					<td><a href="/journal/{l.journal_entry_id}">#{l.journal_entry_id}</a></td>
					<td>
						{l.description}
						{#if l.memo}<div class="muted small">{l.memo}</div>{/if}
					</td>
					<td class="num"><Amount cents={l.debit_cents} blankZero /></td>
					<td class="num"><Amount cents={l.credit_cents} blankZero /></td>
					<td class="num"><Amount cents={l.running_balance_cents} /></td>
				</tr>
			{:else}
				<tr><td colspan="6" class="empty">No postings in this period.</td></tr>
			{/each}
		</tbody>
		<tfoot>
			<tr>
				<td colspan="5">Closing balance</td>
				<td class="num"><Amount cents={data.ledger.closing_balance_cents} currency={account.currency} /></td>
			</tr>
		</tfoot>
	</table>
</div>
