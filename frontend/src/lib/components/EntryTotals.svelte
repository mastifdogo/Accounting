<script lang="ts">
	import type { JournalEntry } from '#lib/api/client.ts';
	import Amount from './Amount.svelte';

	let { totals }: { totals: JournalEntry['totals'] } = $props();
</script>

{#each totals as t, i (t.currency)}{#if i > 0}<br />{/if}<Amount cents={t.amount_cents} currency={t.currency} />{/each}
