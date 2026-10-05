<script lang="ts">
	import { goto, invalidateAll } from '$app/navigation';
	import Amount from '#lib/components/Amount.svelte';
	import ErrorNotice from '#lib/components/ErrorNotice.svelte';
	import { ACCOUNT_TYPES, CURRENCIES, api, unwrap, type AccountType, type Currency } from '#lib/api/client.ts';
	import { naturalBalance } from '#lib/money.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	let showForm = $state(false);
	let code = $state('');
	let name = $state('');
	let type = $state<AccountType>('asset');
	let currency = $state<Currency>('CAD');
	let description = $state('');
	let saving = $state(false);
	let error = $state<unknown>(null);
	let created = $state('');

	async function create(e: SubmitEvent) {
		e.preventDefault();
		saving = true;
		error = null;
		try {
			const acc = unwrap(await api().POST('/accounts', { body: { code, name, type, currency, description } }));
			created = `Created ${acc.code} ${acc.name} (${acc.currency}).`;
			code = name = description = '';
			await invalidateAll();
		} catch (err) {
			error = err;
		} finally {
			saving = false;
		}
	}

	function toggleInactive(e: Event) {
		goto((e.currentTarget as HTMLInputElement).checked ? '?inactive=1' : '?', { reset: false });
	}
</script>

<div class="toolbar">
	<h1 style="margin:0">Accounts</h1>
	<span class="spacer"></span>
	<label class="inline"><input type="checkbox" checked={data.includeInactive} onchange={toggleInactive} /> Show inactive</label>
	<button class="primary" onclick={() => (showForm = !showForm)}>{showForm ? 'Close' : '+ New account'}</button>
</div>

{#if created}<div class="notice ok">{created}</div>{/if}

{#if showForm}
	<form class="card" onsubmit={create}>
		<ErrorNotice {error} />
		<div class="toolbar" style="margin:0">
			<label>Code <input bind:value={code} required maxlength="32" pattern="[A-Za-z0-9][A-Za-z0-9._\-]*" placeholder="1000" size="8" /></label>
			<label style="flex:1">Name <input bind:value={name} required maxlength="200" placeholder="Bank CAD" /></label>
			<label>
				Type
				<select bind:value={type}>
					{#each ACCOUNT_TYPES as t (t)}<option value={t}>{t}</option>{/each}
				</select>
			</label>
			<label>
				Currency
				<select bind:value={currency}>
					{#each CURRENCIES as c (c)}<option value={c}>{c}</option>{/each}
				</select>
			</label>
			<label style="flex:1">Description <input bind:value={description} maxlength="1000" /></label>
			<button class="primary" disabled={saving}>{saving ? 'Saving…' : 'Create'}</button>
		</div>
		<p class="muted small" style="margin-bottom:0">The code and currency cannot be changed later.</p>
	</form>
{/if}

<div class="table-wrap">
	{#if data.accounts.length === 0}
		<div class="empty">No accounts yet.</div>
	{:else}
		<table>
			<thead>
				<tr><th>Code</th><th>Name</th><th>Type</th><th>Currency</th><th class="num">Balance</th><th></th></tr>
			</thead>
			<tbody>
				{#each data.accounts as a (a.code)}
					<tr>
						<td class="mono"><a href="/accounts/{encodeURIComponent(a.code)}">{a.code}</a></td>
						<td>{a.name}{#if a.description}<div class="muted small">{a.description}</div>{/if}</td>
						<td>{a.type}</td>
						<td>{a.currency}</td>
						<td class="num"><Amount cents={naturalBalance(a.type, a.balance_cents)} /></td>
						<td>{#if !a.is_active}<span class="badge warn">inactive</span>{/if}</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</div>
<p class="muted small">Balances use each account's normal side: debit for assets and expenses, credit for liabilities, equity and revenue.</p>
