<script lang="ts">
	import { formatCents } from '#lib/money.ts';

	// Displays integer cents. Zero can be shown as blank (e.g. in debit/credit columns).
	let { cents, currency, blankZero = false }: { cents: number; currency?: string; blankZero?: boolean } = $props();
</script>

{#if !(blankZero && cents === 0)}<span class:neg={cents < 0}
		>{formatCents(cents)}{#if currency}&nbsp;<span class="muted small">{currency}</span>{/if}</span
	>{/if}
