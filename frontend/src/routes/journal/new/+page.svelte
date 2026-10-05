<script lang="ts">
	import { goto } from '$app/navigation';
	import ErrorNotice from '#lib/components/ErrorNotice.svelte';
	import { ApiError, CURRENCIES, api, today, unwrap, type Currency } from '#lib/api/client.ts';
	import { formatCents, parseAmount } from '#lib/money.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	type Line = { key: number; account_code: string; debit: string; credit: string; memo: string };
	let nextKey = 0;
	const blankLine = (): Line => ({ key: nextKey++, account_code: '', debit: '', credit: '', memo: '' });

	let entryDate = $state(today());
	let description = $state('');
	let reference = $state('');
	let lines = $state<Line[]>([blankLine(), blankLine()]);
	let saving = $state(false);
	let error = $state<unknown>(null);

	let accountsByCode = $derived(new Map(data.accounts.map((a) => [a.code, a])));

	// Live per-line parsing and per-currency balance (all integer cents).
	let parsed = $derived(
		lines.map((l) => {
			const debit = parseAmount(l.debit);
			const credit = parseAmount(l.credit);
			const currency = accountsByCode.get(l.account_code)?.currency;
			let problem = '';
			if (debit === null) problem = 'Invalid debit';
			else if (credit === null) problem = 'Invalid credit';
			else if (debit > 0 && credit > 0) problem = 'Debit or credit, not both';
			return { debit: debit ?? 0, credit: credit ?? 0, currency, problem };
		})
	);
	let balances = $derived(
		CURRENCIES.map((c: Currency) => {
			let debit = 0;
			let credit = 0;
			for (const p of parsed) {
				if (p.currency !== c) continue;
				debit += p.debit;
				credit += p.credit;
			}
			return { currency: c, debit, credit, diff: debit - credit };
		}).filter((b) => b.debit !== 0 || b.credit !== 0)
	);
	let usedLines = $derived(parsed.filter((p) => p.debit > 0 || p.credit > 0).length);
	let ready = $derived(
		usedLines >= 2 &&
			balances.length > 0 &&
			balances.every((b) => b.diff === 0) &&
			parsed.every((p) => !p.problem) &&
			lines.every((l, i) => (parsed[i].debit === 0 && parsed[i].credit === 0) || accountsByCode.has(l.account_code))
	);

	// Server-reported problems for each line, from details like "lines[2].account_code".
	let lineErrors = $derived.by(() => {
		const map = new Map<number, string[]>();
		if (error instanceof ApiError) {
			for (const d of error.body.details ?? []) {
				const m = d.field ? /^lines\[(\d+)\]/.exec(d.field) : null;
				if (m) map.set(Number(m[1]), [...(map.get(Number(m[1])) ?? []), d.message]);
			}
		}
		return map;
	});

	function addLine() {
		lines.push(blankLine());
	}

	function removeLine(i: number) {
		if (lines.length > 2) lines.splice(i, 1);
	}

	/** Fill the selected line with whatever balances its currency. */
	function balanceLine(i: number) {
		const cur = parsed[i].currency;
		const b = balances.find((x) => x.currency === cur);
		if (!b) return;
		const own = parsed[i].debit - parsed[i].credit;
		const needed = -(b.diff - own); // debit-positive amount that zeroes the currency
		lines[i].debit = needed > 0 ? formatCents(needed).replaceAll(',', '') : '';
		lines[i].credit = needed < 0 ? formatCents(-needed).replaceAll(',', '') : '';
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		saving = true;
		error = null;
		try {
			// Lines with no amount are dropped; the server re-validates everything.
			const body = {
				entry_date: entryDate,
				description,
				reference,
				lines: lines
					.map((l, i) => ({ l, p: parsed[i] }))
					.filter(({ p }) => p.debit > 0 || p.credit > 0)
					.map(({ l, p }) => ({ account_code: l.account_code, debit_cents: p.debit, credit_cents: p.credit, memo: l.memo }))
			};
			const created = unwrap(await api().POST('/journal-entries', { body }));
			await goto(`/journal/${created.id}`);
		} catch (err) {
			error = err;
		} finally {
			saving = false;
		}
	}
</script>

<h1>New journal entry</h1>

{#if data.accounts.length < 2}
	<div class="notice error">You need at least two active accounts. <a href="/accounts">Create accounts</a>.</div>
{/if}

<form onsubmit={submit}>
	<ErrorNotice {error} />

	<div class="card toolbar">
		<label>Date <input type="date" bind:value={entryDate} required /></label>
		<label style="flex:2">Description <input bind:value={description} required maxlength="500" placeholder="What happened?" /></label>
		<label style="flex:1">Reference <input bind:value={reference} maxlength="100" placeholder="Invoice / receipt #" /></label>
	</div>

	<div class="table-wrap">
		<table class="lines">
			<thead>
				<tr><th style="width:34%">Account</th><th class="num">Debit</th><th class="num">Credit</th><th>Memo</th><th></th></tr>
			</thead>
			<tbody>
				{#each lines as line, i (line.key)}
					{@const p = parsed[i]}
					{@const errs = lineErrors.get(i)}
					<tr>
						<td>
							<select bind:value={line.account_code} aria-label="Account for line {i + 1}" style="width:100%" aria-invalid={errs ? 'true' : undefined}>
								<option value="">Choose account…</option>
								{#each data.accounts as a (a.code)}<option value={a.code}>{a.code} — {a.name} ({a.currency})</option>{/each}
							</select>
							{#if p.problem}<div class="small neg">{p.problem}</div>{/if}
							{#each errs ?? [] as m (m)}<div class="small neg">{m}</div>{/each}
						</td>
						<td class="num">
							<input class="amount" inputmode="decimal" bind:value={line.debit} disabled={p.credit > 0} aria-label="Debit for line {i + 1}" aria-invalid={parseAmount(line.debit) === null ? 'true' : undefined} size="12" placeholder="Debit" />
						</td>
						<td class="num">
							<input class="amount" inputmode="decimal" bind:value={line.credit} disabled={p.debit > 0} aria-label="Credit for line {i + 1}" aria-invalid={parseAmount(line.credit) === null ? 'true' : undefined} size="12" placeholder="Credit" />
						</td>
						<td><input bind:value={line.memo} maxlength="500" aria-label="Memo for line {i + 1}" placeholder="Memo" style="width:100%" /></td>
						<td style="white-space:nowrap">
							<button type="button" class="link" title="Fill the amount that balances this line's currency" onclick={() => balanceLine(i)} disabled={!p.currency}>=</button>
							<button type="button" class="link" title="Remove line" onclick={() => removeLine(i)} disabled={lines.length <= 2}>✕</button>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>

	<div class="toolbar" style="margin-top:0.75rem">
		<button type="button" onclick={addLine}>+ Add line</button>
		<span class="spacer"></span>
		{#each balances as b (b.currency)}
			<span class="badge {b.diff === 0 ? 'ok' : 'danger'}" title="Debits {formatCents(b.debit)} / Credits {formatCents(b.credit)}">
				{b.currency}: {b.diff === 0 ? `balanced ${formatCents(b.debit)}` : `off by ${formatCents(b.diff)}`}
			</span>
		{/each}
		<button class="primary" disabled={!ready || saving}>{saving ? 'Posting…' : 'Post entry'}</button>
	</div>
	<p class="muted small">
		Each currency must balance on its own. For CAD↔USD transfers, post through an FX clearing account in each currency.
		Posted entries cannot be edited; corrections are made with a reversing entry.
	</p>
</form>

<style>
	/* Phones: stack each line as a small card instead of a wide table row. */
	@media (max-width: 720px) {
		.lines thead {
			display: none;
		}
		.lines tr {
			display: grid;
			grid-template-columns: 1fr 1fr auto;
			gap: 0.5rem;
			padding: 0.75rem;
			border-bottom: 1px solid var(--border);
		}
		.lines td {
			border: none;
			padding: 0;
		}
		.lines td:nth-child(1),
		.lines td:nth-child(4) {
			grid-column: 1 / -1;
		}
		.lines td:nth-child(5) {
			grid-column: 3;
			grid-row: 2;
			align-self: center;
		}
		.lines input.amount {
			width: 100%;
		}
	}
</style>
