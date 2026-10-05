<script lang="ts">
	import { invalidateAll } from '$app/navigation';
	import ErrorNotice from '#lib/components/ErrorNotice.svelte';
	import { API_BASE, CURRENCIES, api, unwrap, uploadCsv, type CsvFile, type ImportResult } from '#lib/api/client.ts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	let error = $state<unknown>(null);
	let message = $state('');
	let busy = $state(false);

	// Upload
	let fileInput = $state<HTMLInputElement>();
	let selected = $state<FileList | null>(null);

	// Export
	let from = $state('');
	let to = $state('');
	let currency = $state('');

	async function run(action: () => Promise<string>) {
		busy = true;
		error = null;
		message = '';
		try {
			message = await action();
			await invalidateAll();
		} catch (err) {
			error = err;
		} finally {
			busy = false;
		}
	}

	function upload(e: SubmitEvent) {
		e.preventDefault();
		const file = selected?.[0];
		if (!file) return;
		run(async () => {
			const f: CsvFile = await uploadCsv(file);
			if (fileInput) fileInput.value = '';
			selected = null;
			return `Uploaded ${f.filename} (${size(f.size_bytes)}). Use “Import” to post its entries.`;
		});
	}

	function exportLedger(e: SubmitEvent) {
		e.preventDefault();
		run(async () => {
			const f = unwrap(
				await api().POST('/exports/general-ledger', {
					body: {
						from: from || undefined,
						to: to || undefined,
						currency: currency === 'CAD' || currency === 'USD' ? currency : undefined
					}
				})
			);
			return `Exported ${f.row_count ?? 0} rows to ${f.filename}.`;
		});
	}

	function importFile(name: string) {
		if (!confirm(`Import all journal entries in ${name}? The whole file is posted as one batch, or nothing is if any row is invalid.`)) return;
		run(async () => {
			const r: ImportResult = unwrap(await api().POST('/imports/journal-entries', { body: { filename: name } }));
			return `Imported ${r.entries_imported} entries (${r.lines_imported} lines) from ${r.filename}.`;
		});
	}

	function size(bytes: number): string {
		if (bytes < 1024) return `${bytes} B`;
		if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
		return `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
	}
</script>

<h1>Files</h1>
<p class="muted">CSV files in the server's data directory (the TrueNAS share). Files are never overwritten.</p>

{#if message}<div class="notice ok">{message}</div>{/if}
<ErrorNotice {error} />

<div class="grid">
	<form class="card" onsubmit={upload}>
		<h2 style="margin-top:0">Upload CSV</h2>
		<div class="toolbar" style="margin:0">
			<input type="file" accept=".csv,text/csv" bind:this={fileInput} bind:files={selected} aria-label="CSV file" />
			<button class="primary" disabled={busy || !selected?.length}>Upload</button>
		</div>
		<p class="muted small">Max 10 MiB, UTF-8. Import format:</p>
		<pre class="small mono">entry_key,entry_date,description,account_code,debit,credit
S1,2026-10-01,Cash sale,1000,250.00,
S1,,,4000,,250.00</pre>
		<p class="muted small" style="margin-bottom:0">Optional columns: reference, memo, currency.</p>
	</form>

	<form class="card" onsubmit={exportLedger}>
		<h2 style="margin-top:0">Export General Ledger</h2>
		<div class="toolbar" style="margin:0">
			<label>From <input type="date" bind:value={from} /></label>
			<label>To <input type="date" bind:value={to} /></label>
			<label>
				Currency
				<select bind:value={currency}>
					<option value="">All</option>
					{#each CURRENCIES as c (c)}<option value={c}>{c}</option>{/each}
				</select>
			</label>
			<button class="primary" disabled={busy}>Export</button>
		</div>
		<p class="muted small" style="margin-bottom:0">Writes a UTF-8 CSV file to the share; it then appears below.</p>
	</form>
</div>

<h2>Files on the share</h2>
<div class="table-wrap">
	{#if data.files.length === 0}
		<div class="empty">No CSV files yet.</div>
	{:else}
		<table>
			<thead>
				<tr><th>File</th><th class="num">Size</th><th>Modified</th><th></th></tr>
			</thead>
			<tbody>
				{#each data.files as f (f.filename)}
					<tr>
						<td class="mono">{f.filename}</td>
						<td class="num">{size(f.size_bytes)}</td>
						<td class="small">{new Date(f.modified_at).toLocaleString()}</td>
						<td style="white-space:nowrap; text-align:right">
							<a class="button" href="{API_BASE}/files/{encodeURIComponent(f.filename)}" download>Download</a>
							{#if !f.filename.startsWith('general_ledger_')}
								<button onclick={() => importFile(f.filename)} disabled={busy}>Import</button>
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</div>

<style>
	pre {
		background: var(--bg);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: 0.5rem;
		overflow-x: auto;
	}
</style>
