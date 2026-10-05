<script lang="ts">
	import { goto } from '$app/navigation';
	import EntryTotals from '#lib/components/EntryTotals.svelte';
	import { _PAGE_SIZE } from './+page';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	let from = $state('');
	let to = $state('');
	let accountCode = $state('');
	$effect.pre(() => {
		from = data.filter.from;
		to = data.filter.to;
		accountCode = data.filter.account_code;
	});

	function href(offset: number, f = { from, to, account_code: accountCode }) {
		const q = new URLSearchParams();
		if (f.from) q.set('from', f.from);
		if (f.to) q.set('to', f.to);
		if (f.account_code) q.set('account_code', f.account_code);
		if (offset > 0) q.set('offset', String(offset));
		return `?${q}`;
	}

	function apply(e: SubmitEvent) {
		e.preventDefault();
		goto(href(0), { reset: false });
	}
</script>

<div class="toolbar">
	<h1 style="margin:0">Journal</h1>
	<span class="spacer"></span>
	<a class="button" href="/journal/new">+ New entry</a>
</div>

<form class="toolbar" onsubmit={apply}>
	<label>From <input type="date" bind:value={from} /></label>
	<label>To <input type="date" bind:value={to} /></label>
	<label>
		Account
		<select bind:value={accountCode}>
			<option value="">All accounts</option>
			{#each data.accounts as a (a.code)}<option value={a.code}>{a.code} — {a.name} ({a.currency})</option>{/each}
		</select>
	</label>
	<button>Filter</button>
	{#if data.filter.from || data.filter.to || data.filter.account_code}<a class="button" href="?">Clear</a>{/if}
</form>

<div class="table-wrap">
	{#if data.entries.length === 0}
		<div class="empty">No journal entries match.</div>
	{:else}
		<table>
			<thead>
				<tr><th>Date</th><th>#</th><th>Description</th><th>Reference</th><th>Accounts</th><th class="num">Amount</th><th></th></tr>
			</thead>
			<tbody>
				{#each data.entries as e (e.id)}
					<tr>
						<td class="mono">{e.entry_date}</td>
						<td><a href="/journal/{e.id}">{e.id}</a></td>
						<td>{e.description}</td>
						<td>{e.reference}</td>
						<td class="small mono">{[...new Set(e.lines.map((l) => l.account_code))].join(', ')}</td>
						<td class="num"><EntryTotals totals={e.totals} /></td>
						<td>
							{#if e.reverses_id}<span class="badge">reversal of #{e.reverses_id}</span>{/if}
							{#if e.reversed_by_id}<span class="badge warn">reversed</span>{/if}
							{#if e.import_id}<span class="badge">imported</span>{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</div>

<div class="toolbar" style="margin-top:1rem">
	{#if data.filter.offset > 0}
		<a class="button" href={href(Math.max(0, data.filter.offset - _PAGE_SIZE), data.filter)}>← Newer</a>
	{/if}
	<span class="spacer"></span>
	{#if data.entries.length === _PAGE_SIZE}
		<a class="button" href={href(data.filter.offset + _PAGE_SIZE, data.filter)}>Older →</a>
	{/if}
</div>
