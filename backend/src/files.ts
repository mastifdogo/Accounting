// The CSV directory (a TrueNAS bind mount): General Ledger export, listing,
// safe opening by name, and uploads.

import { constants, promises as fsp } from "node:fs";
import type { FileHandle } from "node:fs/promises";
import type { Stats } from "node:fs";
import path from "node:path";
import { randomInt } from "node:crypto";
import type { App } from "./app.ts";
import { withTx } from "./db.ts";
import { ConflictError, NotFoundError, TooLargeError, ValidationError } from "./errors.ts";
import { formatCents, splitAmount } from "./money.ts";
import { csvRecord } from "./csv.ts";
import { isCurrency, type CsvFile } from "./types.ts";

/**
 * Must match components/schemas/CsvFilename in api/openapi.yaml. Disallows
 * path separators, so a validated name can never escape the CSV directory.
 */
export const CSV_FILENAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.csv$/;

export const MAX_FILE_BYTES = 10 << 20; // 10 MiB, for uploads and imports

/** Column layout of the General Ledger export. */
export const GENERAL_LEDGER_HEADER = [
	"account_code", "account_name", "account_type", "currency",
	"entry_date", "journal_entry_id", "line_id", "line_number",
	"reference", "description", "memo",
	"debit", "credit", "running_balance",
];

/**
 * Neutralises spreadsheet formula injection: text cells starting with
 * = + - @ (or tab / CR) are prefixed with a single quote so spreadsheet apps
 * treat them as text. Numeric columns never pass through here.
 */
export function csvText(s: string): string {
	return s !== "" && "=+-@\t\r".includes(s[0]!) ? "'" + s : s;
}

/** Creates a new, empty file with a random name for writing (mode 0600). */
async function createTemp(dir: string, prefix: string): Promise<{ fh: FileHandle; name: string }> {
	for (let attempt = 0; ; attempt++) {
		const name = path.join(dir, `${prefix}${randomInt(2 ** 32)}.tmp`);
		try {
			return { fh: await fsp.open(name, "wx", 0o600), name };
		} catch (err) {
			if ((err as NodeJS.ErrnoException).code !== "EEXIST" || attempt >= 100) throw err;
		}
	}
}

/** Makes a fully written temp file durable and readable by the ledger group. */
async function finishTemp(fh: FileHandle): Promise<void> {
	await fh.chmod(0o640);
	await fh.sync();
	await fh.close();
}

const GL_SQL = `
	SELECT a.code, a.name, a.type::text AS type, a.currency, x.kind, x.entry_date,
	       x.journal_entry_id, x.line_id, x.line_number,
	       x.reference, x.description, x.memo, x.amount
	  FROM (
	        -- Opening balances (only when a start date is given)
	        SELECT t.account_id, 0 AS kind, $1::date AS entry_date,
	               NULL::bigint AS journal_entry_id, NULL::bigint AS line_id, 0 AS line_number,
	               '' AS reference, 'Opening balance' AS description, '' AS memo,
	               sum(t.amount)::bigint AS amount
	          FROM transactions t
	          JOIN journal_entries e ON e.id = t.journal_entry_id
	         WHERE $1::date IS NOT NULL AND e.entry_date < $1::date
	         GROUP BY t.account_id
	        HAVING sum(t.amount) <> 0
	        UNION ALL
	        -- Postings in range
	        SELECT t.account_id, 1, e.entry_date,
	               e.id, t.id, t.line_number,
	               e.reference, e.description, t.memo, t.amount
	          FROM transactions t
	          JOIN journal_entries e ON e.id = t.journal_entry_id
	         WHERE ($1::date IS NULL OR e.entry_date >= $1::date)
	           AND ($2::date IS NULL OR e.entry_date <= $2::date)
	       ) x
	  JOIN accounts a ON a.id = x.account_id
	 WHERE ($3::text IS NULL OR a.currency = $3)
	 ORDER BY a.code, x.kind, x.entry_date, x.journal_entry_id, x.line_number`;

interface GLRow {
	code: string;
	name: string;
	type: string;
	currency: string;
	kind: number;
	entry_date: string;
	journal_entry_id: bigint | null;
	line_id: bigint | null;
	line_number: number;
	reference: string;
	description: string;
	memo: string;
	amount: bigint;
}

const FETCH_ROWS = 2000;

/**
 * Writes the General Ledger (every posting, grouped by account, with a
 * per-account running balance) to a new UTF-8 CSV file in the CSV directory.
 *
 * When from is set, each account's balance before that date is emitted as an
 * "Opening balance" row so running balances stay correct. Each account has a
 * single currency, so every running balance is in one currency; currency
 * limits the export to accounts in that currency.
 *
 * Rows are streamed from one snapshot through a cursor, so memory stays flat
 * however large the ledger is. The file is written under a temporary name,
 * fsynced, then renamed into place: readers on the share never see a
 * partially written export.
 */
export async function exportGeneralLedger(app: App, from: string | null, to: string | null, currency: string | null): Promise<CsvFile> {
	const v = new ValidationError();
	if (from !== null && to !== null && to < from) v.add("to", "must not be before from");
	if (currency !== null && !isCurrency(currency)) v.add("currency", "must be CAD or USD");
	v.throwIfAny();

	let tmp: { fh: FileHandle; name: string };
	try {
		tmp = await createTemp(app.csvDir, ".general_ledger-");
	} catch (err) {
		throw new Error(`create export file in ${app.csvDir}: ${(err as Error).message}`, { cause: err });
	}
	let committed = false;
	try {
		const rowCount = await withTx(app.db, { isolation: "REPEATABLE READ", readOnly: true }, async (tx) => {
			await tx.query(`DECLARE general_ledger NO SCROLL CURSOR FOR ${GL_SQL}`, [from, to, currency]);
			await tmp.fh.write(csvRecord(GENERAL_LEDGER_HEADER));
			let count = 0;
			let currentCode = "";
			let running = 0n;
			for (;;) {
				const { rows } = await tx.query<GLRow>(`FETCH ${FETCH_ROWS} FROM general_ledger`);
				let chunk = "";
				for (const r of rows) {
					if (r.code !== currentCode || count === 0) {
						currentCode = r.code;
						running = 0n;
					}
					running += r.amount;
					let debit = "";
					let credit = "";
					let lineNo = "";
					if (r.kind === 1) {
						const [d, c] = splitAmount(r.amount);
						if (d > 0n) debit = formatCents(d);
						else credit = formatCents(c);
						lineNo = String(r.line_number);
					}
					chunk += csvRecord([
						csvText(r.code),
						csvText(r.name),
						r.type,
						r.currency,
						r.entry_date,
						r.journal_entry_id === null ? "" : String(r.journal_entry_id),
						r.line_id === null ? "" : String(r.line_id),
						lineNo,
						csvText(r.reference),
						csvText(r.description),
						csvText(r.memo),
						debit,
						credit,
						formatCents(running),
					]);
					count++;
				}
				if (chunk) await tmp.fh.write(chunk);
				if (rows.length < FETCH_ROWS) return count;
			}
		});
		await finishTemp(tmp.fh);

		const now = new Date();
		const stamp = now.toISOString().replace(/[-:]/g, "").replace(/\.(\d{3})Z$/, "Z");
		const name = `general_ledger_${stamp}_${String(now.getUTCMilliseconds()).padStart(3, "0")}.csv`;
		const final = path.join(app.csvDir, name);
		await fsp.rename(tmp.name, final);
		committed = true;

		const info = await fsp.stat(final);
		return { filename: name, size_bytes: info.size, modified_at: info.mtime, row_count: rowCount };
	} finally {
		if (!committed) {
			await tmp.fh.close().catch(() => {});
			await fsp.rm(tmp.name, { force: true });
		}
	}
}

/** The CSV files in the CSV directory, newest first. */
export async function listCsvFiles(app: App): Promise<CsvFile[]> {
	let entries;
	try {
		entries = await fsp.readdir(app.csvDir, { withFileTypes: true });
	} catch (err) {
		throw new Error(`read CSV directory: ${(err as Error).message}`, { cause: err });
	}
	const files: CsvFile[] = [];
	for (const e of entries) {
		if (!e.isFile() || !CSV_FILENAME_RE.test(e.name)) continue;
		try {
			const info = await fsp.lstat(path.join(app.csvDir, e.name));
			if (info.isFile()) files.push({ filename: e.name, size_bytes: info.size, modified_at: info.mtime });
		} catch {
			// removed between readdir and stat
		}
	}
	files.sort((a, b) => b.modified_at.getTime() - a.modified_at.getTime());
	return files;
}

/**
 * Opens a CSV file for reading. The name is validated (no path separators)
 * and symlinks are refused, so the open cannot escape the CSV directory.
 */
export async function openCsvFile(app: App, name: string): Promise<{ fh: FileHandle; info: Stats }> {
	if (!CSV_FILENAME_RE.test(name)) throw new NotFoundError();
	let fh: FileHandle;
	try {
		fh = await fsp.open(path.join(app.csvDir, name), constants.O_RDONLY | constants.O_NOFOLLOW);
	} catch (err) {
		const code = (err as NodeJS.ErrnoException).code;
		if (code === "ENOENT" || code === "ELOOP" || code === "ENOTDIR") throw new NotFoundError();
		throw new Error(`open ${name}: ${(err as Error).message}`, { cause: err });
	}
	const info = await fh.stat().catch(() => null);
	if (!info || !info.isFile()) {
		await fh.close();
		throw new NotFoundError();
	}
	return { fh, info };
}

const BOM = Buffer.from([0xef, 0xbb, 0xbf]);

/** Strips a UTF-8 byte order mark (Excel adds one). */
export function stripBOM(b: Buffer): Buffer {
	return b.subarray(0, 3).equals(BOM) ? b.subarray(3) : b;
}

/** Decodes strict UTF-8, or returns null if the bytes are not valid UTF-8. */
export function decodeUTF8(b: Buffer): string | null {
	try {
		return new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(b);
	} catch {
		return null;
	}
}

/**
 * Stores an uploaded CSV file in the CSV directory under name. Existing files
 * are never overwritten (409), and the file appears atomically: it is written
 * to a temporary file, fsynced, then hard-linked into place, which fails if
 * the name is already taken.
 */
export async function saveUploadedCsv(app: App, name: string, data: Buffer): Promise<CsvFile> {
	const v = new ValidationError();
	if (!CSV_FILENAME_RE.test(name)) {
		v.add("file", "file name must end in .csv and use only letters, digits, '.', '_' or '-' (max 128 characters)");
		throw v;
	}
	if (data.length > MAX_FILE_BYTES) throw new TooLargeError();
	if (data.length === 0) {
		v.add("file", "file is empty");
		throw v;
	}
	const body = stripBOM(data);
	if (decodeUTF8(body) === null || body.includes(0)) {
		v.add("file", "file must be UTF-8 text");
		throw v;
	}

	const exists = () => new ConflictError(`a file named ${JSON.stringify(name)} already exists`);
	const final = path.join(app.csvDir, name);
	if (await fsp.lstat(final).then(() => true, () => false)) throw exists();

	let tmp: { fh: FileHandle; name: string };
	try {
		tmp = await createTemp(app.csvDir, ".upload-");
	} catch (err) {
		throw new Error(`create upload file in ${app.csvDir}: ${(err as Error).message}`, { cause: err });
	}
	try {
		try {
			await tmp.fh.writeFile(data);
			await finishTemp(tmp.fh);
		} catch (err) {
			await tmp.fh.close().catch(() => {});
			throw err;
		}
		try {
			await fsp.link(tmp.name, final);
		} catch (err) {
			if ((err as NodeJS.ErrnoException).code === "EEXIST") throw exists();
			// Some network filesystems do not support hard links. Fall back to
			// rename, which is atomic but would replace a file created in the
			// tiny window since the check above.
			if (await fsp.lstat(final).then(() => true, () => false)) throw exists();
			await fsp.rename(tmp.name, final);
		}
	} finally {
		await fsp.rm(tmp.name, { force: true }); // after a link this only removes the temp name
	}
	const info = await fsp.stat(final);
	return { filename: name, size_bytes: info.size, modified_at: info.mtime };
}
