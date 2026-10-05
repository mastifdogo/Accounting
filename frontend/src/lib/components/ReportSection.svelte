<script lang="ts">
	import type { ReportLine } from '#lib/api/client.ts';
	import Amount from './Amount.svelte';

	// One titled block of a financial statement with a total row.
	let {
		title,
		lines,
		total,
		currency,
		extra
	}: { title: string; lines: ReportLine[]; total: number; currency: string; extra?: { label: string; cents: number } } =
		$props();
</script>

<tbody>
	<tr class="heading"><th colspan="2">{title}</th></tr>
	{#each lines as l (l.account_code)}
		<tr>
			<td><a class="mono" href="/accounts/{encodeURIComponent(l.account_code)}">{l.account_code}</a> {l.account_name}</td>
			<td class="num"><Amount cents={l.amount_cents} /></td>
		</tr>
	{/each}
	{#if extra}
		<tr>
			<td><em>{extra.label}</em></td>
			<td class="num"><Amount cents={extra.cents} /></td>
		</tr>
	{/if}
	{#if lines.length === 0 && !extra}
		<tr><td colspan="2" class="muted">None</td></tr>
	{/if}
	<tr class="total">
		<td>Total {title.toLowerCase()}</td>
		<td class="num"><Amount cents={total} {currency} /></td>
	</tr>
</tbody>

<style>
	.heading th {
		background: none;
		font-size: 0.85rem;
		padding-top: 1rem;
		color: var(--text);
	}
	.total td {
		font-weight: 600;
		border-top: 1px solid var(--border);
	}
</style>
