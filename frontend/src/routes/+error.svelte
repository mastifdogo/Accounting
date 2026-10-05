<script lang="ts">
	import { page } from '$app/state';
</script>

{#if page.status === 404}
	<h1>Not found</h1>
	<p class="muted">That page or record doesn't exist.</p>
{:else}
	<h1>Something went wrong</h1>
	<div class="notice error">{page.error?.message ?? 'Unknown error'}</div>
{/if}
<p><a href="/">Back to the dashboard</a></p>
