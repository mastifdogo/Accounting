<script lang="ts">
	import { goto } from '$app/navigation';
	import Amount from '#lib/components/Amount.svelte';
	import { CURRENCIES } from '#lib/api/client.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	let asOf = $state('');
	let currency = $state('');
	$effect.pre(() => {
		asOf = data.asOf;
		currency = data.currency;
	});

	let groups = $derived(
		data.tb.totals.map((t) => ({ total: t, rows: data.tb.rows.filter((r) => r.currency === t.currency) }))
	);

	function apply(e: SubmitEvent) {
		e.preventDefault();
		const q = new URLSearchParams({ as_of: asOf });
		if (currency) q.set('currency', currency);
		goto(`?${q}`, { reset: false });
	}
</script>

<h1>Trial balance</h1>

<form class="toolbar" onsubmit={apply}>
	<label>As of <input type="date" bind:value={asOf} required /></label>
	<label>
		Currency
		<select bind:value={currency}>
			<option value="">All</option>
			{#each CURRENCIES as c (c)}<option value={c}>{c}</option>{/each}
		</select>
	</label>
	<button>Show</button>
	<span class="spacer"></span>
	<button type="button" onclick={() => window.print()}>Print</button>
</form>

{#each groups as g (g.total.currency)}
	<h2>
		{g.total.currency}
		{#if g.total.debit_cents === g.total.credit_cents}
			<span class="badge ok">balanced</span>
		{:else}
			<span class="badge danger">out of balance</span>
		{/if}
	</h2>
	<div class="table-wrap">
		<table>
			<thead>
				<tr><th>Code</th><th>Account</th><th>Type</th><th class="num">Debit</th><th class="num">Credit</th></tr>
			</thead>
			<tbody>
				{#each g.rows as r (r.account_code)}
					<tr>
						<td class="mono"><a href="/accounts/{encodeURIComponent(r.account_code)}?to={data.asOf}">{r.account_code}</a></td>
						<td>{r.account_name}</td>
						<td>{r.account_type}</td>
						<td class="num"><Amount cents={r.debit_cents} blankZero /></td>
						<td class="num"><Amount cents={r.credit_cents} blankZero /></td>
					</tr>
				{/each}
			</tbody>
			<tfoot>
				<tr>
					<td colspan="3">Total ({g.total.currency})</td>
					<td class="num"><Amount cents={g.total.debit_cents} /></td>
					<td class="num"><Amount cents={g.total.credit_cents} /></td>
				</tr>
			</tfoot>
		</table>
	</div>
{:else}
	<div class="card empty">No postings on or before {data.asOf}.</div>
{/each}
