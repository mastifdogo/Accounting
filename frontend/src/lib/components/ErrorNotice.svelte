<script lang="ts">
	import { describeError } from '#lib/api/client.ts';

	let { error }: { error: unknown } = $props();
	let info = $derived(error ? describeError(error) : null);
</script>

{#if info}
	<div class="notice error" role="alert">
		<strong>{info.message}</strong>
		{#if info.details.length}
			<ul>
				{#each info.details as d (d)}
					<li>{d}</li>
				{/each}
			</ul>
		{/if}
	</div>
{/if}
