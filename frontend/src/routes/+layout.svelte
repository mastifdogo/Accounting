<script lang="ts">
	import '../app.css';
	import favicon from '#lib/assets/favicon.svg';
	import { page } from '$app/state';
	import type { LayoutProps } from './$types';

	let { children }: LayoutProps = $props();

	const links = [
		{ href: '/', label: 'Dashboard', exact: true },
		{ href: '/journal', label: 'Journal', exact: true },
		{ href: '/journal/new', label: 'New entry', exact: true },
		{ href: '/accounts', label: 'Accounts', exact: false },
		{ href: '/reports/trial-balance', label: 'Trial balance', exact: false },
		{ href: '/files', label: 'Files', exact: false }
	];

	function active(href: string, exact: boolean): boolean {
		const p = page.url.pathname;
		if (exact) return p === href || (href === '/journal' && /^\/journal\/\d+$/.test(p));
		return p === href || p.startsWith(href + '/');
	}
</script>

<svelte:head>
	<link rel="icon" href={favicon} />
	<title>Ledger</title>
</svelte:head>

<header>
	<nav aria-label="Main">
		<a class="brand" href="/">Ledger</a>
		{#each links as l (l.href)}
			<a href={l.href} aria-current={active(l.href, l.exact) ? 'page' : undefined}>{l.label}</a>
		{/each}
	</nav>
</header>

<main>
	{@render children()}
</main>

<style>
	header {
		background: var(--surface);
		border-bottom: 1px solid var(--border);
		position: sticky;
		top: 0;
		z-index: 10;
	}
	nav {
		max-width: 1200px;
		margin: 0 auto;
		padding: 0 1rem;
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 1.25rem;
		align-items: center;
		min-height: 3.25rem;
	}
	nav a {
		color: var(--muted);
		padding: 0.4rem 0;
		border-bottom: 2px solid transparent;
	}
	nav a:hover {
		color: var(--text);
		text-decoration: none;
	}
	nav a[aria-current='page'] {
		color: var(--text);
		border-bottom-color: var(--accent);
	}
	nav a.brand {
		color: var(--text);
		font-weight: 700;
		margin-right: 0.75rem;
	}
</style>
