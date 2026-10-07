import type { App } from "./app.ts";
import { withTx, queryRow, queryRows, toNumber, type Queryable } from "./db.ts";
import { ConflictError, NotFoundError, ValidationError } from "./errors.ts";
import { MAX_CENTS, formatCents, splitAmount } from "./money.ts";
import { checkText, quote, trimSpace } from "./text.ts";
import { dateYear, today } from "./date.ts";
import {
	ACCOUNT_CODE_RE,
	type Currency,
	type CurrencyTotal,
	type JournalEntry,
	type JournalEntryCreate,
	type JournalLine,
	type JournalLineCreate,
	type ReverseRequest,
} from "./types.ts";

const MIN_LINES_PER_ENTRY = 2;
const MAX_LINES_PER_ENTRY = 1000;

/**
 * How validators name fields in error messages, so the same checks can report
 * "lines[1].account_code" for the JSON API and "row 7.account_code" for CSV
 * imports.
 */
export interface FieldNames {
	entry(field: string): string;
	line(i: number, field: string): string;
}

export const jsonFieldNames: FieldNames = {
	entry: (f) => f,
	line: (i, f) => (f === "" ? `lines[${i}]` : `lines[${i}].${f}`),
};

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

/**
 * Checks the double-entry rules that need no database access, so callers get
 * precise, per-field errors. Per-currency balance is checked once account
 * currencies are known (checkLinesPostable), and the database re-checks the
 * critical invariants with constraints and triggers.
 */
export function validateJournalEntry(e: JournalEntryCreate): void {
	const v = new ValidationError();
	validateEntry(v, e, jsonFieldNames);
	v.throwIfAny();
}

export function validateEntry(v: ValidationError, e: JournalEntryCreate, fn: FieldNames): void {
	const start = v.size;

	if (e.entry_date === "") {
		v.add(fn.entry("entry_date"), "is required");
	} else {
		const y = dateYear(e.entry_date);
		if (y < 1900 || y > 9999) v.add(fn.entry("entry_date"), "year must be between 1900 and 9999");
	}
	checkText(v, fn.entry("description"), e.description, 1, 500);
	checkText(v, fn.entry("reference"), e.reference, 0, 100);

	const n = e.lines.length;
	if (n < MIN_LINES_PER_ENTRY) {
		v.add(fn.entry("lines"), `a journal entry needs at least ${MIN_LINES_PER_ENTRY} lines, got ${n}`);
	} else if (n > MAX_LINES_PER_ENTRY) {
		v.add(fn.entry("lines"), `a journal entry may have at most ${MAX_LINES_PER_ENTRY} lines, got ${n}`);
		return; // don't produce thousands of per-line errors
	}

	let totalDebit = 0n;
	let totalCredit = 0n;
	let overflow = false;
	for (const [i, l] of e.lines.entries()) {
		if (!ACCOUNT_CODE_RE.test(l.account_code)) {
			if (l.account_code === "") v.add(fn.line(i, "account_code"), "is required");
			else v.add(fn.line(i, "account_code"), `${quote(l.account_code)} is not a valid account code`);
		}
		checkText(v, fn.line(i, "memo"), l.memo, 0, 500);

		if (l.debit_cents < 0n || l.credit_cents < 0n) {
			v.add(fn.line(i, ""), "debit and credit must not be negative");
			continue;
		}
		if (l.debit_cents > MAX_CENTS || l.credit_cents > MAX_CENTS) {
			v.add(fn.line(i, ""), `amount exceeds the maximum of ${MAX_CENTS} cents`);
			continue;
		}
		if (l.debit_cents > 0n && l.credit_cents > 0n) {
			v.add(fn.line(i, ""), "a line must be either a debit or a credit, not both");
			continue;
		}
		if (l.debit_cents === 0n && l.credit_cents === 0n) {
			v.add(fn.line(i, ""), "a line must have a non-zero debit or credit");
			continue;
		}
		totalDebit += l.debit_cents;
		totalCredit += l.credit_cents;
		if (totalDebit > MAX_CENTS || totalCredit > MAX_CENTS) {
			overflow = true;
			break;
		}
	}

	// Balancing per currency implies balancing overall, so an overall
	// mismatch can be reported before account currencies are known.
	if (overflow) {
		v.add(fn.entry("lines"), `entry total exceeds the maximum of ${MAX_CENTS} cents`);
	} else if (v.size === start && totalDebit !== totalCredit) {
		v.add(
			fn.entry("lines"),
			`entry is unbalanced: debits ${formatCents(totalDebit)}, credits ${formatCents(totalCredit)} (difference ${formatCents(totalDebit - totalCredit)})`,
		);
	}
}

// ---------------------------------------------------------------------------
// Account resolution (codes -> ids, inside the posting transaction)
// ---------------------------------------------------------------------------

export interface PostingAccount {
	id: bigint;
	currency: Currency;
	active: boolean;
}

/**
 * Loads the given account codes and takes a share lock on them, so an account
 * cannot be deactivated by a concurrent request before the posting
 * transaction commits.
 */
export async function lockAccounts(tx: Queryable, codes: string[]): Promise<Map<string, PostingAccount>> {
	const rows = await queryRows<{ code: string; id: bigint; currency: Currency; is_active: boolean }>(
		tx,
		`SELECT code, id, currency, is_active
		   FROM accounts WHERE code = ANY($1)
		  ORDER BY id FOR SHARE`,
		[codes],
	);
	return new Map(rows.map((r) => [r.code, { id: r.id, currency: r.currency, active: r.is_active }]));
}

export function lineCodes(lines: JournalLineCreate[]): string[] {
	return lines.map((l) => l.account_code);
}

export function sortedKeys<V>(m: Map<Currency, V>): Currency[] {
	return [...m.keys()].sort();
}

/**
 * Verifies every line's account exists and is active, and that the entry
 * balances within each currency.
 */
export function checkLinesPostable(
	v: ValidationError,
	e: JournalEntryCreate,
	accounts: Map<string, PostingAccount>,
	fn: FieldNames,
): void {
	const start = v.size;
	const sums = new Map<Currency, bigint>();
	for (const [i, l] of e.lines.entries()) {
		const acc = accounts.get(l.account_code);
		if (!acc) {
			v.add(fn.line(i, "account_code"), `account ${quote(l.account_code)} does not exist`);
		} else if (!acc.active) {
			v.add(fn.line(i, "account_code"), `account ${quote(l.account_code)} is inactive`);
		} else {
			sums.set(acc.currency, (sums.get(acc.currency) ?? 0n) + l.debit_cents - l.credit_cents);
		}
	}
	if (v.size > start) return;
	for (const c of sortedKeys(sums)) {
		const d = sums.get(c)!;
		if (d !== 0n) {
			v.add(
				fn.entry("lines"),
				`entry is unbalanced in ${c}: debits minus credits = ${formatCents(d)}; ` +
					"post cross-currency amounts through an FX clearing account in each currency",
			);
		}
	}
}

// ---------------------------------------------------------------------------
// Writing journal entries
// ---------------------------------------------------------------------------

/**
 * Validates a journal entry and writes the header and all of its lines
 * atomically:
 *
 *   BEGIN
 *     lock referenced accounts (FOR SHARE); check existence, activity and
 *     per-currency balance
 *     INSERT journal_entries ...
 *     INSERT transactions ... (all lines, one statement)
 *   COMMIT  <- deferred triggers verify >= 2 lines and per-currency zero sum
 *
 * Any failure, including the COMMIT-time balance check, rolls everything back.
 */
export async function createJournalEntry(app: App, e: JournalEntryCreate, actorId: number | null): Promise<JournalEntry> {
	validateJournalEntry(e);
	const id = await withTx(app.db, {}, async (tx) => {
		const accounts = await lockAccounts(tx, lineCodes(e.lines));
		const v = new ValidationError();
		checkLinesPostable(v, e, accounts, jsonFieldNames);
		v.throwIfAny();
		return insertJournalEntry(tx, e, accounts, null, null, actorId);
	});
	return getJournalEntry(app, id);
}

/**
 * Writes the header and its lines inside tx and returns the new entry id.
 * Must only be called inside a transaction, with every line's account present
 * in accounts.
 */
export async function insertJournalEntry(
	tx: Queryable,
	e: JournalEntryCreate,
	accounts: Map<string, PostingAccount>,
	reversesId: bigint | null,
	importId: bigint | null,
	actorId: number | null,
): Promise<bigint> {
	const row = await queryRow<{ id: bigint }>(
		tx,
		`INSERT INTO journal_entries (entry_date, description, reference, reverses_id, import_id, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		[e.entry_date, trimSpace(e.description), trimSpace(e.reference), reversesId, importId, actorId],
	);
	const id = row!.id;

	const n = e.lines.length;
	const accountIds: bigint[] = [];
	const currencies: string[] = [];
	const lineNumbers: number[] = [];
	const amounts: bigint[] = [];
	const memos: string[] = [];
	for (const [i, l] of e.lines.entries()) {
		const acc = accounts.get(l.account_code);
		if (!acc) throw new Error(`insert transaction lines: account ${quote(l.account_code)} was not resolved`);
		accountIds.push(acc.id);
		currencies.push(acc.currency);
		lineNumbers.push(i + 1);
		amounts.push(l.debit_cents - l.credit_cents); // debit > 0, credit < 0
		memos.push(trimSpace(l.memo));
	}

	const r = await tx.query(
		`INSERT INTO transactions (journal_entry_id, account_id, currency, line_number, amount, memo)
		 SELECT $1, l.account_id, l.currency, l.line_number, l.amount, l.memo
		   FROM unnest($2::bigint[], $3::text[], $4::int[], $5::bigint[], $6::text[])
		        AS l(account_id, currency, line_number, amount, memo)`,
		[id, accountIds, currencies, lineNumbers, amounts, memos],
	);
	if (r.rowCount !== n) throw new Error(`insert transaction lines: wrote ${r.rowCount} of ${n} lines`);
	return id;
}

/**
 * Writes many journal entries inside tx with three statements in total (one
 * to reserve ids, one for the headers, one for all lines), and returns their
 * ids in order. Ids ascend in the order given, exactly as one-by-one inserts
 * would assign them. Used by CSV imports, where tens of thousands of
 * single-entry round trips would be slow. The deferred triggers still check
 * every entry at COMMIT.
 */
export async function insertJournalEntries(
	tx: Queryable,
	entries: JournalEntryCreate[],
	accounts: Map<string, PostingAccount>,
	importId: bigint | null,
	actorId: number | null,
): Promise<bigint[]> {
	if (entries.length === 0) return [];
	const ids = (
		await queryRows<{ id: bigint }>(
			tx,
			`SELECT nextval(pg_get_serial_sequence('journal_entries', 'id')) AS id
			   FROM generate_series(1, $1) ORDER BY id`,
			[entries.length],
		)
	).map((r) => r.id);

	await tx.query(
		`INSERT INTO journal_entries (id, entry_date, description, reference, import_id, created_by)
		 OVERRIDING SYSTEM VALUE
		 SELECT h.id, h.entry_date, h.description, h.reference, $5, $6
		   FROM unnest($1::bigint[], $2::date[], $3::text[], $4::text[]) AS h(id, entry_date, description, reference)`,
		[ids, entries.map((e) => e.entry_date), entries.map((e) => trimSpace(e.description)), entries.map((e) => trimSpace(e.reference)), importId, actorId],
	);

	const entryIds: bigint[] = [];
	const accountIds: bigint[] = [];
	const currencies: string[] = [];
	const lineNumbers: number[] = [];
	const amounts: bigint[] = [];
	const memos: string[] = [];
	for (const [n, e] of entries.entries()) {
		for (const [i, l] of e.lines.entries()) {
			const acc = accounts.get(l.account_code);
			if (!acc) throw new Error(`insert transaction lines: account ${quote(l.account_code)} was not resolved`);
			entryIds.push(ids[n]!);
			accountIds.push(acc.id);
			currencies.push(acc.currency);
			lineNumbers.push(i + 1);
			amounts.push(l.debit_cents - l.credit_cents); // debit > 0, credit < 0
			memos.push(trimSpace(l.memo));
		}
	}
	const r = await tx.query(
		`INSERT INTO transactions (journal_entry_id, account_id, currency, line_number, amount, memo)
		 SELECT l.journal_entry_id, l.account_id, l.currency, l.line_number, l.amount, l.memo
		   FROM unnest($1::bigint[], $2::bigint[], $3::text[], $4::int[], $5::bigint[], $6::text[])
		        AS l(journal_entry_id, account_id, currency, line_number, amount, memo)`,
		[entryIds, accountIds, currencies, lineNumbers, amounts, memos],
	);
	if (r.rowCount !== entryIds.length) throw new Error(`insert transaction lines: wrote ${r.rowCount} of ${entryIds.length} lines`);
	return ids;
}

/**
 * Posts a new entry that swaps every debit and credit of the original, in one
 * transaction. Reversals cannot themselves be reversed, and an entry can be
 * reversed only once (also enforced by a UNIQUE index).
 */
export async function reverseJournalEntry(app: App, id: bigint, req: ReverseRequest, actorId: number | null): Promise<JournalEntry> {
	const orig = await getJournalEntry(app, id);
	if (orig.reverses_id !== null) {
		throw new ConflictError(`entry ${id} is itself a reversal and cannot be reversed`);
	}
	if (orig.reversed_by_id !== null) {
		throw new ConflictError(`entry ${id} was already reversed by entry ${orig.reversed_by_id}`);
	}

	const e: JournalEntryCreate = {
		entry_date: req.entry_date ?? today(),
		description: trimSpace(req.description ?? "") !== "" ? req.description! : `Reversal of entry #${id}`,
		reference: orig.reference,
		lines: orig.lines.map((l) => ({
			account_code: l.account_code,
			debit_cents: l.credit_cents,
			credit_cents: l.debit_cents,
			memo: l.memo,
		})),
	};
	validateJournalEntry(e);

	const newId = await withTx(app.db, {}, async (tx) => {
		const accounts = await lockAccounts(tx, lineCodes(e.lines));
		const v = new ValidationError();
		checkLinesPostable(v, e, accounts, jsonFieldNames);
		v.throwIfAny();
		return insertJournalEntry(tx, e, accounts, id, null, actorId);
	});
	return getJournalEntry(app, newId);
}

// ---------------------------------------------------------------------------
// Reading journal entries
// ---------------------------------------------------------------------------

const ENTRY_SELECT = `
	SELECT e.id, e.entry_date, e.description, e.reference, e.reverses_id,
	       r.id AS reversed_by_id, e.import_id, u.username, e.posted_at
	  FROM journal_entries e
	  LEFT JOIN journal_entries r ON r.reverses_id = e.id
	  LEFT JOIN users u ON u.id = e.created_by`;

interface EntryRow {
	id: bigint;
	entry_date: string;
	description: string;
	reference: string;
	reverses_id: bigint | null;
	reversed_by_id: bigint | null;
	import_id: bigint | null;
	username: string | null;
	posted_at: Date;
}

function toEntry(r: EntryRow): JournalEntry {
	return {
		id: Number(r.id),
		entry_date: r.entry_date,
		description: r.description,
		reference: r.reference,
		reverses_id: toNumber(r.reverses_id),
		reversed_by_id: toNumber(r.reversed_by_id),
		import_id: toNumber(r.import_id),
		created_by: r.username,
		posted_at: r.posted_at,
		totals: [],
		lines: [],
	};
}

export async function getJournalEntry(app: App, id: bigint): Promise<JournalEntry> {
	const row = await queryRow<EntryRow>(app.db, ENTRY_SELECT + ` WHERE e.id = $1`, [id]);
	if (!row) throw new NotFoundError();
	const e = toEntry(row);
	await attachLines(app, [e]);
	return e;
}

export interface EntryFilter {
	from: string | null;
	to: string | null;
	accountCode: string | null;
	limit: number;
	offset: number;
}

export async function listJournalEntries(app: App, f: EntryFilter): Promise<JournalEntry[]> {
	const rows = await queryRows<EntryRow>(
		app.db,
		ENTRY_SELECT +
			`
		WHERE ($1::date IS NULL OR e.entry_date >= $1)
		  AND ($2::date IS NULL OR e.entry_date <= $2)
		  AND ($3::text IS NULL OR EXISTS (
		        SELECT 1 FROM transactions t JOIN accounts a ON a.id = t.account_id
		         WHERE t.journal_entry_id = e.id AND a.code = $3))
		ORDER BY e.entry_date DESC, e.id DESC
		LIMIT $4 OFFSET $5`,
		[f.from, f.to, f.accountCode, f.limit, f.offset],
	);
	const entries = rows.map(toEntry);
	await attachLines(app, entries);
	return entries;
}

/** Loads lines for all given entries in one query and computes each entry's per-currency totals. */
async function attachLines(app: App, entries: JournalEntry[]): Promise<void> {
	if (entries.length === 0) return;
	const byId = new Map(entries.map((e) => [e.id, e]));
	const rows = await queryRows<{
		journal_entry_id: bigint;
		id: bigint;
		line_number: number;
		code: string;
		name: string;
		currency: Currency;
		amount: bigint;
		memo: string;
	}>(
		app.db,
		`SELECT t.journal_entry_id, t.id, t.line_number, a.code, a.name, t.currency, t.amount, t.memo
		   FROM transactions t
		   JOIN accounts a ON a.id = t.account_id
		  WHERE t.journal_entry_id = ANY($1)
		  ORDER BY t.journal_entry_id, t.line_number`,
		[entries.map((e) => e.id)],
	);
	const totals = new Map<number, Map<Currency, bigint>>();
	for (const r of rows) {
		const entryId = Number(r.journal_entry_id);
		const [debit, credit] = splitAmount(r.amount);
		const line: JournalLine = {
			id: Number(r.id),
			line_number: r.line_number,
			account_code: r.code,
			account_name: r.name,
			currency: r.currency,
			debit_cents: debit,
			credit_cents: credit,
			memo: r.memo,
		};
		byId.get(entryId)!.lines.push(line);
		let t = totals.get(entryId);
		if (!t) totals.set(entryId, (t = new Map()));
		t.set(r.currency, (t.get(r.currency) ?? 0n) + debit);
	}
	for (const [id, m] of totals) {
		byId.get(id)!.totals = sortedKeys(m).map((c): CurrencyTotal => ({ currency: c, amount_cents: m.get(c)! }));
	}
}
