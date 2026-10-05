<script lang="ts">
	import { goto } from '$app/navigation';
	import Amount from '#lib/components/Amount.svelte';
	import ReportSection from '#lib/components/ReportSection.svelte';
	import { CURRENCIES } from '#lib/api/client.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	let asOf = $state('');
	let currency = $state('');
	$effect.pre(() => {
		asOf = data.asOf;
		currency = data.currency;
	});

	function apply(e: SubmitEvent) {
		e.preventDefault();
		const q = new URLSearchParams({ as_of: asOf });
		if (currency) q.set('currency', currency);
		goto(`?${q}`, { reset: false });
	}
</script>

<p class="small"><a href="/reports">← Reports</a></p>
<h1>Balance sheet</h1>

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

<div class="grid">
	{#each data.bs.currencies as s (s.currency)}
		<section class="table-wrap">
			<table>
				<caption>
					<strong>{s.currency}</strong> · as of {data.bs.as_of}
					{#if s.balanced}<span class="badge ok">balanced</span>{:else}<span class="badge danger">out of balance</span>{/if}
				</caption>
				<ReportSection title="Assets" lines={s.assets} total={s.total_assets_cents} currency={s.currency} />
				<ReportSection title="Liabilities" lines={s.liabilities} total={s.total_liabilities_cents} currency={s.currency} />
				<ReportSection
					title="Equity"
					lines={s.equity}
					total={s.total_equity_cents}
					currency={s.currency}
					extra={s.net_income_cents !== 0 ? { label: 'Net income (unclosed)', cents: s.net_income_cents } : undefined}
				/>
				<tfoot>
					<tr>
						<td>Liabilities + equity</td>
						<td class="num"><Amount cents={s.total_liabilities_cents + s.total_equity_cents} currency={s.currency} /></td>
					</tr>
				</tfoot>
			</table>
		</section>
	{:else}
		<div class="card empty">No postings on or before {data.bs.as_of}.</div>
	{/each}
</div>

<style>
	caption {
		text-align: left;
		padding: 0.75rem;
		font-size: 1.05rem;
	}
</style>
