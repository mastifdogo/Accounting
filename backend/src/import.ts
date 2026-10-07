// All-or-nothing CSV import of journal entries.
//
// Columns: entry_key, entry_date, description, account_code, debit, credit
// (required) and reference, memo, currency (optional). Header names are
// case-insensitive; column order is free. Rows sharing an entry_key form one
// journal entry and must be contiguous. See /imports/journal-entries in
// api/openapi.yaml.

import { createHash } from "node:crypto";
import type { App } from "./app.ts";
import { withTx, queryRow } from "./db.ts";
import { ConflictError, ValidationError } from "./errors.ts";
import { parseCents } from "./money.ts";
import { parseDate } from "./date.ts";
import { charCount, quote, trimSpace } from "./text.ts";
import { CsvParseError, readCsv } from "./csv.ts";
import { MAX_FILE_BYTES, decodeUTF8, openCsvFile, stripBOM } from "./files.ts";
import { checkLinesPostable, insertJournalEntries, lineCodes, lockAccounts, validateEntry, type FieldNames } from "./ledger.ts";
import { isCurrency, type ImportResult, type JournalEntryCreate } from "./types.ts";

const MAX_IMPORT_ROWS = 50_000;
const MAX_ENTRY_KEY_LEN = 100;

const REQUIRED_COLUMNS = ["entry_key", "entry_date", "description", "account_code", "debit", "credit"];
const OPTIONAL_COLUMNS = ["reference", "memo", "currency"];

export interface ImportGroup {
	key: string;
	firstRow: number;
	entry: JournalEntryCreate;
	/** File line number of each journal line. */
	rows: number[];
	/** Optional asserted currency of each line ("" = none). */
	currency: string[];
}

function groupFieldNames(g: ImportGroup): FieldNames {
	return {
		entry: (f) => `entry_key ${g.key} (row ${g.firstRow}).${f}`,
		line: (i, f) => (f === "" ? `row ${g.rows[i]}` : `row ${g.rows[i]}.${f}`),
	};
}

function tooLarge(): ValidationError {
	const v = new ValidationError();
	v.add("filename", `file is larger than ${MAX_FILE_BYTES} bytes`);
	return v;
}

/**
 * Imports every journal entry in a CSV file from the CSV directory in ONE
 * database transaction: either the whole file is posted or nothing is. A file
 * whose exact content was imported before is refused.
 */
export async function importJournalEntries(app: App, filename: string, actorId: number | null): Promise<ImportResult> {
	const { fh, info } = await openCsvFile(app, filename);
	let raw: Buffer;
	try {
		if (info.size > MAX_FILE_BYTES) throw tooLarge();
		raw = await fh.readFile();
	} finally {
		await fh.close();
	}
	if (raw.length > MAX_FILE_BYTES) throw tooLarge();
	const digest = createHash("sha256").update(raw).digest("hex");

	const groups = parseImportCSV(raw);
	const lineCount = groups.reduce((n, g) => n + g.entry.lines.length, 0);

	const importId = await withTx(app.db, {}, async (tx) => {
		// Friendly duplicate check; the UNIQUE constraint still guards races.
		const prev = await queryRow<{ filename: string; imported_at: Date }>(
			tx,
			`SELECT filename, imported_at FROM csv_imports WHERE sha256 = $1`,
			[digest],
		);
		if (prev) {
			const at = prev.imported_at.toISOString().replace(/\.\d{3}Z$/, "Z");
			throw new ConflictError(`this file's content was already imported as ${quote(prev.filename)} on ${at}`);
		}

		const accounts = await lockAccounts(tx, groups.flatMap((g) => lineCodes(g.entry.lines)));
		const v = new ValidationError();
		for (const g of groups) {
			const fn = groupFieldNames(g);
			for (const [i, l] of g.entry.lines.entries()) {
				const acc = accounts.get(l.account_code);
				const asserted = g.currency[i]!;
				if (acc && asserted !== "" && asserted !== acc.currency) {
					v.add(fn.line(i, "currency"), `account ${quote(l.account_code)} is in ${acc.currency}, not ${asserted}`);
				}
			}
			checkLinesPostable(v, g.entry, accounts, fn);
		}
		v.throwIfAny();

		const row = await queryRow<{ id: bigint }>(
			tx,
			`INSERT INTO csv_imports (filename, sha256, entry_count, line_count, imported_by)
			 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			[filename, digest, groups.length, lineCount, actorId],
		);
		await insertJournalEntries(tx, groups.map((g) => g.entry), accounts, row!.id, actorId);
		return row!.id;
	});

	return {
		import_id: Number(importId),
		filename,
		sha256: digest,
		entries_imported: groups.length,
		lines_imported: lineCount,
	};
}

/**
 * Turns the file into journal entries and runs every check that needs no
 * database access. All problems are reported together.
 */
export function parseImportCSV(raw: Buffer): ImportGroup[] {
	const v = new ValidationError();
	const text = decodeUTF8(stripBOM(raw)); // tolerate Excel's BOM
	if (text === null) {
		v.add("", "file is not valid UTF-8");
		throw v;
	}

	const records = readCsv(text);
	let header: string[];
	try {
		const first = records.next();
		if (first.done) {
			v.add("", "file is empty");
			throw v;
		}
		header = first.value.fields;
	} catch (err) {
		if (err instanceof CsvParseError) {
			v.add("row 1", err.message);
			throw v;
		}
		throw err;
	}

	const known = new Set([...REQUIRED_COLUMNS, ...OPTIONAL_COLUMNS]);
	const col = new Map<string, number>();
	header.forEach((h, i) => {
		const name = trimSpace(h).toLowerCase();
		if (!known.has(name)) v.add("row 1", `unknown column ${quote(h)}`);
		else if (col.has(name)) v.add("row 1", `duplicate column ${quote(h)}`);
		else col.set(name, i);
	});
	for (const c of REQUIRED_COLUMNS) {
		if (!col.has(c)) v.add("row 1", `missing required column ${quote(c)}`);
	}
	v.throwIfAny();
	const get = (rec: string[], name: string): string => {
		const i = col.get(name);
		return i === undefined ? "" : trimSpace(rec[i]!);
	};

	const groups: ImportGroup[] = [];
	const seen = new Set<string>();
	let current: ImportGroup | undefined;
	let rows = 0;
	for (;;) {
		let next: IteratorResult<{ fields: string[]; line: number }>;
		try {
			next = records.next();
		} catch (err) {
			if (err instanceof CsvParseError) {
				v.add("", err.message);
				throw v;
			}
			throw err;
		}
		if (next.done) break;
		const { fields: rec, line } = next.value;
		if (++rows > MAX_IMPORT_ROWS) {
			v.add("", `file has more than ${MAX_IMPORT_ROWS} data rows`);
			throw v;
		}
		if (rec.join("") === "") continue; // skip blank lines
		const rowField = (f: string) => `row ${line}.${f}`;

		const key = get(rec, "entry_key");
		if (key === "") {
			v.add(rowField("entry_key"), "is required");
			continue;
		}
		if (charCount(key) > MAX_ENTRY_KEY_LEN) {
			v.add(rowField("entry_key"), `must be at most ${MAX_ENTRY_KEY_LEN} characters`);
			continue;
		}

		const date = get(rec, "entry_date");
		const description = get(rec, "description");
		const reference = get(rec, "reference");

		if (current === undefined || current.key !== key) {
			if (seen.has(key)) {
				v.add(rowField("entry_key"), `rows for entry_key ${quote(key)} must be contiguous`);
				continue;
			}
			seen.add(key);
			let d = "";
			try {
				d = parseDate(date);
			} catch (err) {
				v.add(rowField("entry_date"), (err as Error).message);
			}
			current = { key, firstRow: line, entry: { entry_date: d, description, reference, lines: [] }, rows: [], currency: [] };
			groups.push(current);
		} else {
			// Later rows may leave header fields blank, but may not contradict them.
			if (date !== "" && date !== current.entry.entry_date && current.entry.entry_date !== "") {
				v.add(rowField("entry_date"), `differs from the first row of entry_key ${quote(key)}`);
			}
			if (description !== "" && description !== current.entry.description) {
				v.add(rowField("description"), `differs from the first row of entry_key ${quote(key)}`);
			}
			if (reference !== "" && reference !== current.entry.reference) {
				v.add(rowField("reference"), `differs from the first row of entry_key ${quote(key)}`);
			}
		}

		let debit = 0n;
		let credit = 0n;
		try {
			debit = parseCents(get(rec, "debit"));
		} catch (err) {
			v.add(rowField("debit"), (err as Error).message);
		}
		try {
			credit = parseCents(get(rec, "credit"));
		} catch (err) {
			v.add(rowField("credit"), (err as Error).message);
		}
		const cur = get(rec, "currency").toUpperCase();
		if (cur !== "" && !isCurrency(cur)) v.add(rowField("currency"), "must be CAD or USD");

		current.entry.lines.push({ account_code: get(rec, "account_code"), debit_cents: debit, credit_cents: credit, memo: get(rec, "memo") });
		current.rows.push(line);
		current.currency.push(cur);
	}

	if (groups.length === 0 && v.size === 0) v.add("", "file contains no journal entries");
	for (const g of groups) validateEntry(v, g.entry, groupFieldNames(g));
	v.throwIfAny();
	return groups;
}
