<script lang="ts">
	import Amount from '#lib/components/Amount.svelte';
	import EntryTotals from '#lib/components/EntryTotals.svelte';
	import { ACCOUNT_TYPES, CURRENCIES, type AccountType, type Currency } from '#lib/api/client.ts';
	import { naturalBalance } from '#lib/money.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const typeLabels: Record<AccountType, string> = {
		asset: 'Assets',
		liability: 'Liabilities',
		equity: 'Equity',
		revenue: 'Revenue',
		expense: 'Expenses'
	};

	// Per-currency totals of natural balances by account type. Currencies are
	// never added together.
	let summary = $derived(
		CURRENCIES.map((currency: Currency) => {
			const byType = Object.fromEntries(ACCOUNT_TYPES.map((t) => [t, 0])) as Record<AccountType, number>;
			let count = 0;
			for (const a of data.accounts) {
				if (a.currency !== currency) continue;
				count++;
				byType[a.type] += naturalBalance(a.type, a.balance_cents);
			}
			return { currency, byType, count, netIncome: byType.revenue - byType.expense };
		}).filter((s) => s.count > 0)
	);
</script>

<h1>Dashboard</h1>

{#if data.accounts.length === 0}
	<div class="card">
		<p><strong>Welcome!</strong> Start by creating your chart of accounts.</p>
		<p>
			Each account has one currency (CAD or USD). For CAD↔USD transfers, create an FX clearing account
			(type <em>equity</em>, e.g. “Currency trading CAD/USD”) in each currency.
		</p>
		<a class="button" href="/accounts">Set up accounts →</a>
	</div>
{:else}
	<div class="grid">
		{#each summary as s (s.currency)}
			<section class="card">
				<h2 style="margin-top:0">{s.currency}</h2>
				<table>
					<tbody>
						{#each ACCOUNT_TYPES as t (t)}
							<tr>
								<td>{typeLabels[t]}</td>
								<td class="num"><Amount cents={s.byType[t]} /></td>
							</tr>
						{/each}
					</tbody>
					<tfoot>
						<tr>
							<td>Net income</td>
							<td class="num"><Amount cents={s.netIncome} currency={s.currency} /></td>
						</tr>
					</tfoot>
				</table>
			</section>
		{/each}
	</div>
{/if}

<div class="toolbar">
	<h2 style="margin:0">Recent entries</h2>
	<span class="spacer"></span>
	<a class="button" href="/journal/new">+ New entry</a>
</div>

<div class="table-wrap">
	{#if data.entries.length === 0}
		<div class="empty">No journal entries yet.</div>
	{:else}
		<table>
			<thead>
				<tr><th>Date</th><th>#</th><th>Description</th><th class="num">Amount</th></tr>
			</thead>
			<tbody>
				{#each data.entries as e (e.id)}
					<tr>
						<td class="mono">{e.entry_date}</td>
						<td><a href="/journal/{e.id}">{e.id}</a></td>
						<td>{e.description}</td>
						<td class="num"><EntryTotals totals={e.totals} /></td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</div>
