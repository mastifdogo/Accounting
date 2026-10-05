<script lang="ts">
	import { goto } from '$app/navigation';
	import Amount from '#lib/components/Amount.svelte';
	import ReportSection from '#lib/components/ReportSection.svelte';
	import { CURRENCIES } from '#lib/api/client.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	let from = $state('');
	let to = $state('');
	let currency = $state('');
	$effect.pre(() => {
		// The server fills in defaults (year to date), so show what it used.
		from = data.is.from;
		to = data.is.to;
		currency = data.currency;
	});

	function apply(e: SubmitEvent) {
		e.preventDefault();
		const q = new URLSearchParams({ from, to });
		if (currency) q.set('currency', currency);
		goto(`?${q}`, { reset: false });
	}

	function preset(kind: 'ytd' | 'last-year' | 'this-month') {
		const now = new Date();
		const y = now.getFullYear();
		const pad = (n: number) => String(n).padStart(2, '0');
		const fmt = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
		if (kind === 'ytd') [from, to] = [`${y}-01-01`, fmt(now)];
		if (kind === 'last-year') [from, to] = [`${y - 1}-01-01`, `${y - 1}-12-31`];
		if (kind === 'this-month') [from, to] = [fmt(new Date(y, now.getMonth(), 1)), fmt(new Date(y, now.getMonth() + 1, 0))];
		const q = new URLSearchParams({ from, to });
		if (currency) q.set('currency', currency);
		goto(`?${q}`, { reset: false });
	}
</script>

<p class="small"><a href="/reports">← Reports</a></p>
<h1>Income statement</h1>

<form class="toolbar" onsubmit={apply}>
	<label>From <input type="date" bind:value={from} required /></label>
	<label>To <input type="date" bind:value={to} required /></label>
	<label>
		Currency
		<select bind:value={currency}>
			<option value="">All</option>
			{#each CURRENCIES as c (c)}<option value={c}>{c}</option>{/each}
		</select>
	</label>
	<button>Show</button>
	<button type="button" class="link" onclick={() => preset('this-month')}>This month</button>
	<button type="button" class="link" onclick={() => preset('ytd')}>Year to date</button>
	<button type="button" class="link" onclick={() => preset('last-year')}>Last year</button>
	<span class="spacer"></span>
	<button type="button" onclick={() => window.print()}>Print</button>
</form>

<div class="grid">
	{#each data.is.currencies as s (s.currency)}
		<section class="table-wrap">
			<table>
				<caption><strong>{s.currency}</strong> · {data.is.from} to {data.is.to}</caption>
				<ReportSection title="Revenue" lines={s.revenue} total={s.total_revenue_cents} currency={s.currency} />
				<ReportSection title="Expenses" lines={s.expenses} total={s.total_expenses_cents} currency={s.currency} />
				<tfoot>
					<tr>
						<td>Net income</td>
						<td class="num"><Amount cents={s.net_income_cents} currency={s.currency} /></td>
					</tr>
				</tfoot>
			</table>
		</section>
	{:else}
		<div class="card empty">No revenue or expenses between {data.is.from} and {data.is.to}.</div>
	{/each}
</div>

<style>
	caption {
		text-align: left;
		padding: 0.75rem;
		font-size: 1.05rem;
	}
</style>
