import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile, readdir, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import { expectStatus, multipartBody, sessionFrom, setupTestApp, type TestApp } from "./harness.ts";
import { insertJournalEntry, lockAccounts } from "../src/ledger.ts";
import { GENERAL_LEDGER_HEADER, MAX_FILE_BYTES } from "../src/files.ts";
import { readCsv } from "../src/csv.ts";
import { createUser, setUserActive, SESSION_COOKIE } from "../src/auth.ts";
import { LOGIN_MAX_FAILURES } from "../src/limiter.ts";

type Line = { account_code: string; debit_cents: number; credit_cents: number; memo?: string };
const dr = (code: string, cents: number): Line => ({ account_code: code, debit_cents: cents, credit_cents: 0 });
const cr = (code: string, cents: number): Line => ({ account_code: code, debit_cents: 0, credit_cents: cents });
const entry = (date: string, description: string, ...lines: Line[]) => ({ entry_date: date, description, lines });

async function createAccount(ta: TestApp, code: string, name: string, type: string, currency: string): Promise<void> {
	expectStatus(await ta.call("POST", "/accounts", { code, name, type, currency }), 201);
}

const readRecords = (text: string) => [...readCsv(text)].map((r) => r.fields);

test("ledger end to end", async (t) => {
	const ta = await setupTestApp(t);
	if (!ta) return;
	const { app, call, countRows } = ta;

	await createAccount(ta, "1000", "Bank CAD", "asset", "CAD");
	await createAccount(ta, "1010", "Bank USD", "asset", "USD");
	await createAccount(ta, "1900", "FX clearing CAD", "asset", "CAD");
	await createAccount(ta, "1910", "FX clearing USD", "asset", "USD");
	await createAccount(ta, "4000", "Sales", "revenue", "CAD");
	await createAccount(ta, "6100", "=Supplies", "expense", "CAD"); // formula-looking name
	await createAccount(ta, "9000", "Old account", "expense", "CAD");

	expectStatus(await call("POST", "/accounts", { code: "1000", name: "Dup", type: "asset", currency: "CAD" }), 409);
	expectStatus(await call("POST", "/accounts", { code: "2000", name: "Euro", type: "asset", currency: "EUR" }), 422);
	expectStatus(await call("PATCH", "/accounts/9000", { is_active: false }), 200);
	expectStatus(await call("PATCH", "/accounts/9000", { code: "9001" }), 400);
	expectStatus(await call("GET", "/accounts/nope"), 404);

	let r = await call("GET", "/currencies");
	expectStatus(r, 200);
	assert.ok(r.text.includes('"CAD"') && r.text.includes('"USD"'), r.text);

	// --- Balanced entries post successfully -------------------------------
	r = await call("POST", "/journal-entries", entry("2026-09-15", "Cash sale", dr("1000", 150000), { ...cr("4000", 150000), memo: "Invoice 1 — café ☕" }));
	expectStatus(r, 201);
	const sale = r.json();
	assert.deepEqual(sale.totals, [{ currency: "CAD", amount_cents: 150000 }]);
	assert.equal(sale.lines.length, 2);
	assert.equal(sale.lines[1].credit_cents, 150000);
	assert.equal(sale.lines[1].account_code, "4000");
	assert.equal(sale.lines[1].currency, "CAD");

	r = await call("POST", "/journal-entries", entry("2026-10-02", "Supplies", { ...dr("6100", 4599), memo: "+paper" }, cr("1000", 4599)));
	expectStatus(r, 201);
	const purchase = r.json();

	// Convert CAD 1,370.00 to USD 1,000.00 through the FX clearing accounts.
	r = await call("POST", "/journal-entries", entry("2026-10-03", "Buy USD", dr("1900", 137000), cr("1000", 137000), dr("1010", 100000), cr("1910", 100000)));
	expectStatus(r, 201);
	assert.deepEqual(r.json().totals, [
		{ currency: "CAD", amount_cents: 137000 },
		{ currency: "USD", amount_cents: 100000 },
	]);

	const entriesBefore = await countRows("journal_entries");
	const linesBefore = await countRows("transactions");

	// --- Rejected entries leave no trace ----------------------------------
	const rejected: [string, unknown, number][] = [
		["unbalanced", entry("2026-10-03", "Bad", dr("1000", 100), cr("4000", 99)), 422],
		["balanced overall but not per currency", entry("2026-10-03", "Bad", dr("1000", 100), cr("1010", 100)), 422],
		["single line", entry("2026-10-03", "Bad", dr("1000", 100)), 422],
		["inactive account", entry("2026-10-03", "Bad", dr("9000", 100), cr("1000", 100)), 422],
		["unknown account", entry("2026-10-03", "Bad", dr("7777", 100), cr("1000", 100)), 422],
		[
			"account id instead of code",
			{ entry_date: "2026-10-03", description: "Bad", lines: [{ account_id: 1, debit_cents: 1, credit_cents: 0 }, { account_id: 2, debit_cents: 0, credit_cents: 1 }] },
			400,
		],
		[
			"float amount",
			{ entry_date: "2026-10-03", description: "Bad", lines: [{ account_code: "1000", debit_cents: 1.5, credit_cents: 0 }, { account_code: "4000", debit_cents: 0, credit_cents: 1.5 }] },
			400,
		],
		["unknown field", { entry_date: "2026-10-03", description: "Bad", status: "draft", lines: [dr("1000", 1), cr("4000", 1)] }, 400],
	];
	for (const [name, body, want] of rejected) {
		await t.test("reject " + name, async () => expectStatus(await call("POST", "/journal-entries", body), want));
	}
	assert.equal(await countRows("journal_entries"), entriesBefore, "rejected entries changed the ledger");
	assert.equal(await countRows("transactions"), linesBefore, "rejected entries changed the ledger");

	// Resolve accounts for the direct-insert tests below (bypassing the API).
	const accounts = await lockAccounts(app.db, ["1000", "1010", "4000"]);

	// --- Atomicity: a failing line rolls back the header ------------------
	await t.test("rollback on failing line", async () => {
		const tx = await app.db.connect();
		try {
			await tx.query("BEGIN");
			const bad = new Map([
				["1000", accounts.get("1000")!],
				["ghost", { id: 424242n, currency: "CAD" as const, active: true }],
			]);
			await assert.rejects(
				insertJournalEntry(tx, { entry_date: "2026-10-04", description: "Partial", reference: "", lines: [{ account_code: "1000", debit_cents: 5n, credit_cents: 0n, memo: "" }, { account_code: "ghost", debit_cents: 0n, credit_cents: 5n, memo: "" }] }, bad, null, null, null),
			);
			await tx.query("ROLLBACK");
		} finally {
			tx.release();
		}
		assert.equal(await countRows("journal_entries"), entriesBefore, "header survived a failed line");
	});

	// --- Database rejects unbalanced commits even without the app checks ---
	const sneaky: Record<string, [string, bigint][]> = {
		unbalanced: [["1000", 1000n], ["4000", -1n]],
		"cross-currency mix": [["1000", 1000n], ["1010", -1000n]],
	};
	for (const [name, lines] of Object.entries(sneaky)) {
		await t.test("db rejects " + name, async () => {
			const tx = await app.db.connect();
			try {
				await tx.query("BEGIN");
				await insertJournalEntry(
					tx,
					{
						entry_date: "2026-10-04",
						description: "Sneaky",
						reference: "",
						lines: lines.map(([code, amt]) => ({ account_code: code, debit_cents: amt > 0n ? amt : 0n, credit_cents: amt < 0n ? -amt : 0n, memo: "" })),
					},
					accounts,
					null,
					null,
					null,
				);
				await assert.rejects(tx.query("COMMIT"), /unbalanced/);
			} finally {
				tx.release();
			}
			assert.equal(await countRows("journal_entries"), entriesBefore, "unbalanced entry was committed");
		});
	}

	// --- Immutability ------------------------------------------------------
	await t.test("posted rows are immutable", async () => {
		for (const stmt of [
			"UPDATE transactions SET amount = amount * 2",
			"DELETE FROM transactions",
			"UPDATE journal_entries SET description = 'changed'",
			"DELETE FROM journal_entries",
			"TRUNCATE transactions CASCADE",
			"UPDATE accounts SET code = 'X1000' WHERE code = '1000'",
			"UPDATE accounts SET currency = 'USD' WHERE code = '1000'",
		]) {
			await assert.rejects(app.db.query(stmt), Error, stmt);
		}
		// Appending balanced lines to an already-posted entry is also refused.
		await assert.rejects(
			app.db.query(
				`INSERT INTO transactions (journal_entry_id, account_id, currency, line_number, amount)
				 VALUES ($1, $2, 'CAD', 10, 1), ($1, $3, 'CAD', 11, -1)`,
				[sale.id, accounts.get("1000")!.id, accounts.get("4000")!.id],
			),
			/already posted/,
		);
	});

	// --- Reversal ----------------------------------------------------------
	r = await call("POST", `/journal-entries/${purchase.id}/reverse`, { entry_date: "2026-10-05" });
	expectStatus(r, 201);
	const rev = r.json();
	assert.equal(rev.reverses_id, purchase.id);
	assert.equal(rev.lines[0].credit_cents, 4599);
	assert.equal(rev.lines[1].debit_cents, 4599);
	expectStatus(await call("POST", `/journal-entries/${purchase.id}/reverse`), 409);
	expectStatus(await call("POST", `/journal-entries/${rev.id}/reverse`), 409);

	r = await call("GET", `/journal-entries/${purchase.id}`);
	expectStatus(r, 200);
	assert.equal(r.json().reversed_by_id, rev.id);

	// --- Reports -----------------------------------------------------------
	r = await call("GET", "/reports/trial-balance?as_of=2026-12-31");
	expectStatus(r, 200);
	assert.deepEqual(r.json().totals, [
		{ currency: "CAD", debit_cents: 150000, credit_cents: 150000 },
		{ currency: "USD", debit_cents: 100000, credit_cents: 100000 },
	]);
	r = await call("GET", "/reports/trial-balance?as_of=2026-12-31&currency=usd");
	expectStatus(r, 200);
	let tb = r.json();
	assert.equal(tb.rows.length, 2);
	assert.equal(tb.totals.length, 1);
	assert.equal(tb.totals[0].currency, "USD");

	r = await call("GET", "/accounts/1000/ledger?from=2026-10-01");
	expectStatus(r, 200);
	const led = r.json();
	assert.equal(led.account.currency, "CAD");
	assert.equal(led.opening_balance_cents, 150000);
	assert.equal(led.lines.length, 3);
	assert.equal(led.lines[0].running_balance_cents, 150000 - 4599);
	assert.equal(led.closing_balance_cents, 150000 - 137000);

	r = await call("GET", "/journal-entries?limit=2&account_code=1000");
	expectStatus(r, 200);
	const page = r.json();
	assert.equal(page.data.length, 2);
	assert.equal(page.data[0].id, rev.id);

	// --- General Ledger CSV export ----------------------------------------
	r = await call("POST", "/exports/general-ledger");
	expectStatus(r, 201);
	const exp = r.json();
	const raw = await readFile(path.join(app.csvDir, exp.filename));
	assert.ok(raw[0] !== 0xef, "export must be UTF-8 without BOM");
	const text = raw.toString("utf8");
	const records = readRecords(text);
	assert.deepEqual(records[0], GENERAL_LEDGER_HEADER);
	assert.equal(exp.row_count, 10);
	assert.equal(records.length, 11);
	// Account 1000 rows come first: +1500.00, -45.99, -1370.00, +45.99.
	const wantRunning = ["1500.00", "1454.01", "84.01", "130.00"];
	for (const [i, rec] of records.slice(1, 5).entries()) {
		assert.equal(rec[0], "1000");
		assert.equal(rec[3], "CAD");
		assert.equal(rec[13], wantRunning[i]);
	}
	assert.ok(text.includes("Invoice 1 — café ☕"), "UTF-8 memo not preserved");
	assert.ok(text.includes("'=Supplies") && text.includes("'+paper"), "formula-looking text was not neutralised");

	// Date-filtered export emits opening balances; currency filter applies.
	r = await call("POST", "/exports/general-ledger", { from: "2026-10-01", currency: "CAD" });
	expectStatus(r, 201);
	const exp2 = r.json();
	const text2 = await readFile(path.join(app.csvDir, exp2.filename), "utf8");
	const recs2 = readRecords(text2);
	assert.equal(recs2[1]![9], "Opening balance");
	assert.equal(recs2[1]![13], "1500.00");
	assert.equal(recs2[1]![11], "");
	assert.ok(!text2.includes(",USD,"), "CAD-only export contains USD rows");
	expectStatus(await call("POST", "/exports/general-ledger", { currency: "EUR" }), 422);
	expectStatus(await call("POST", "/exports/general-ledger", { from: "2026-10-02", to: "2026-10-01" }), 422);

	assert.deepEqual((await readdir(app.csvDir)).filter((f) => f.endsWith(".tmp")), [], "temp files left behind");

	// --- File listing and download ----------------------------------------
	r = await call("GET", "/files");
	expectStatus(r, 200);
	assert.equal(r.json().data.length, 2);
	r = await call("GET", "/files/" + exp.filename);
	expectStatus(r, 200);
	assert.ok(r.body.equals(raw), "downloaded file differs from export");
	assert.equal(r.headers["content-type"], "text/csv; charset=utf-8");
	assert.equal(r.headers["content-disposition"], `attachment; filename="${exp.filename}"`);
	for (const bad of ["..%2Fetc%2Fpasswd", "missing.csv", ".hidden.csv", "x.txt"]) {
		expectStatus(await call("GET", "/files/" + bad), 404);
	}
});

test("import journal entries", async (t) => {
	const ta = await setupTestApp(t);
	if (!ta) return;
	const { app, call, countRows } = ta;

	await createAccount(ta, "1000", "Bank CAD", "asset", "CAD");
	await createAccount(ta, "1010", "Bank USD", "asset", "USD");
	await createAccount(ta, "1900", "FX clearing CAD", "asset", "CAD");
	await createAccount(ta, "1910", "FX clearing USD", "asset", "USD");
	await createAccount(ta, "4000", "Sales", "revenue", "CAD");

	const write = (name: string, content: string) => writeFile(path.join(app.csvDir, name), content, { mode: 0o600 });
	const importFile = (name: string) => call("POST", "/imports/journal-entries", { filename: name });

	const header = "entry_key,entry_date,description,reference,account_code,debit,credit,memo,currency\n";
	await write(
		"good.csv",
		header +
			"S1,2026-10-01,Sale,INV-1,1000,250.00,,,CAD\n" +
			"S1,,,,4000,,250.00,Widgets,\n" +
			"FX1,2026-10-02,Buy USD,,1900,137.00,,,\n" +
			"FX1,2026-10-02,Buy USD,,1000,,137.00,,\n" +
			"FX1,2026-10-02,Buy USD,,1010,100.00,,,USD\n" +
			"FX1,2026-10-02,Buy USD,,1910,,100.00,,USD\n",
	);

	let r = await importFile("good.csv");
	expectStatus(r, 201);
	const res = r.json();
	assert.equal(res.entries_imported, 2);
	assert.equal(res.lines_imported, 6);
	assert.equal(res.sha256.length, 64);
	const linked = await app.db.query(`SELECT min(import_id) AS id FROM journal_entries`);
	assert.equal(Number(linked.rows[0].id), res.import_id, "entries not linked to import");

	// Same content under another name is refused.
	await write("copy.csv", await readFile(path.join(app.csvDir, "good.csv"), "utf8"));
	r = await importFile("copy.csv");
	expectStatus(r, 409);
	assert.ok(r.text.includes("already imported"), r.text);

	const entries = await countRows("journal_entries");
	const lines = await countRows("transactions");

	// A file with one bad entry imports nothing.
	const cases: Record<string, string> = {
		"one bad entry": header + "A,2026-10-05,Good,,1000,1.00,,,\nA,,,,4000,,1.00,,\n" + "B,2026-10-05,Bad,,1000,1.00,,,\nB,,,,4000,,0.99,,\n",
		"unknown account": header + "A,2026-10-05,x,,1000,1.00,,,\nA,,,,7777,,1.00,,\n",
		"per-currency": header + "A,2026-10-05,x,,1000,1.00,,,\nA,,,,1010,,1.00,,\n",
		"currency mismatch": header + "A,2026-10-05,x,,1000,1.00,,,USD\nA,,,,4000,,1.00,,\n",
	};
	for (const [name, content] of Object.entries(cases)) {
		await t.test(name, async () => {
			const fn = name.replaceAll(" ", "_") + ".csv";
			await write(fn, content);
			const r = await importFile(fn);
			expectStatus(r, 422);
			assert.ok(r.text.includes('"row ') || r.text.includes("entry_key"), `errors should point at rows: ${r.text}`);
		});
	}
	assert.equal(await countRows("journal_entries"), entries, "failed imports changed the ledger");
	assert.equal(await countRows("transactions"), lines);
	assert.equal(await countRows("csv_imports"), 1);

	expectStatus(await importFile("missing.csv"), 404);
	expectStatus(await importFile("../etc/passwd"), 422);

	// Round-trip sanity: the imported amounts appear in the trial balance.
	r = await call("GET", "/reports/trial-balance?as_of=2026-12-31");
	expectStatus(r, 200);
	const tb = r.json();
	assert.equal(tb.totals.length, 2);
	assert.equal(tb.totals[0].debit_cents, 25000);
	assert.equal(tb.totals[1].debit_cents, 10000);
});

test("upload and request safety", async (t) => {
	const ta = await setupTestApp(t);
	if (!ta) return;
	const { app, call, request } = ta;
	await createAccount(ta, "1000", "Bank CAD", "asset", "CAD");
	await createAccount(ta, "4000", "Sales", "revenue", "CAD");

	const cookie = `${SESSION_COOKIE}=${ta.token}`;
	const upload = (filename: string, content: Buffer, headers: Record<string, string> = {}) => {
		const { body, contentType } = multipartBody(filename, content);
		return request("POST", "/api/v1/files", { rawBody: body, cookie, headers: { "Content-Type": contentType, ...headers } });
	};
	const content = Buffer.from("entry_key,entry_date,description,account_code,debit,credit\nS1,2026-10-01,Sale,1000,5.00,\nS1,,,4000,,5.00\n");

	// Upload, then import the uploaded file.
	let r = await upload("sales.csv", content);
	expectStatus(r, 201);
	const f = r.json();
	assert.equal(f.filename, "sales.csv");
	assert.equal(f.size_bytes, content.length);
	assert.ok((await readFile(path.join(app.csvDir, "sales.csv"))).equals(content), "stored file differs from upload");
	assert.equal((await stat(path.join(app.csvDir, "sales.csv"))).mode & 0o777, 0o640);
	expectStatus(await call("POST", "/imports/journal-entries", { filename: "sales.csv" }), 201);

	// Never overwrite; reject bad names, binary content, empty and huge files.
	expectStatus(await upload("sales.csv", Buffer.from("x")), 409);
	expectStatus(await upload("evil.sh", content), 422);
	expectStatus(await upload(".hidden.csv", content), 422);
	// Path components are stripped: the file lands inside the CSV directory.
	expectStatus(await upload("../../escape.csv", content), 201);
	await stat(path.join(app.csvDir, "escape.csv"));
	expectStatus(await upload("bin.csv", Buffer.from([0xff, 0xfe, 0x00, 0x01])), 422);
	expectStatus(await upload("empty.csv", Buffer.alloc(0)), 422);
	r = await upload("huge.csv", Buffer.alloc(MAX_FILE_BYTES + 1, "a"));
	expectStatus(r, 413);
	assert.ok(r.text.includes("payload_too_large"), r.text);
	await assert.rejects(stat(path.join(path.dirname(app.csvDir), "escape.csv")), Error, "upload escaped the CSV directory");
	assert.deepEqual((await readdir(app.csvDir)).filter((f) => f.endsWith(".tmp")), [], "temp files left behind");
	expectStatus(await request("POST", "/api/v1/files", { rawBody: "x", cookie, headers: { "Content-Type": "text/plain" } }), 400);

	// Cross-site browser requests are refused.
	expectStatus(await upload("csrf.csv", content, { "Sec-Fetch-Site": "cross-site" }), 403);
	expectStatus(await upload("csrf2.csv", content, { Origin: "http://evil.example" }), 403);
	expectStatus(await upload("same.csv", content, { "Sec-Fetch-Site": "same-origin" }), 201);
	const port = new URL(`http://${(await request("GET", "/api/v1/health")).headers.host ?? "x"}`).port; // unused; Host is set by the client
	void port;

	// JSON endpoints require application/json (a text/plain form post is a
	// CORS "simple request" a malicious page could send).
	r = await request("POST", "/api/v1/accounts", {
		rawBody: '{"code":"5000","name":"X","type":"expense","currency":"CAD"}',
		cookie,
		headers: { "Content-Type": "text/plain" },
	});
	expectStatus(r, 415);
	expectStatus(await request("POST", "/api/v1/accounts", { rawBody: "x".repeat((1 << 20) + 1), cookie, headers: { "Content-Type": "application/json" } }), 413);
	expectStatus(await request("POST", "/api/v1/accounts", { rawBody: "", cookie, headers: { "Content-Type": "application/json" } }), 400);
	expectStatus(await request("POST", "/api/v1/accounts", { rawBody: "{} {}", cookie, headers: { "Content-Type": "application/json" } }), 400);

	// Non-API paths serve the frontend (or its placeholder); unknown API paths are JSON 404s.
	for (const [p, want] of [["/", 200], ["/journal/new", 200], ["/api/v2/nope", 404], ["/api/v1/nope", 404], ["/api/v1", 404]] as const) {
		const r = await request("GET", p);
		assert.equal(r.status, want, `GET ${p}`);
		if (p.startsWith("/api/")) assert.ok(r.text.includes('"not_found"'), `GET ${p}: expected JSON error, got ${r.text}`);
		assert.equal(r.headers["x-frame-options"], "DENY", `GET ${p}: missing security headers`);
	}
	r = await request("DELETE", "/api/v1/accounts", { cookie });
	expectStatus(r, 405);
	r = await request("POST", "/", {});
	expectStatus(r, 405);
});

test("authentication", async (t) => {
	const ta = await setupTestApp(t);
	if (!ta) return;
	const { app, request } = ta;
	const raw = (method: string, p: string, body?: string, cookie?: string) =>
		request(method, "/api/v1" + p, { rawBody: body, cookie, headers: body ? { "Content-Type": "application/json" } : {} });

	// Health is public; everything else needs a session.
	expectStatus(await raw("GET", "/health"), 200);
	for (const p of ["/accounts", "/journal-entries", "/files", "/auth/me", "/reports/balance-sheet"]) {
		const r = await raw("GET", p);
		assert.equal(r.status, 401, `GET ${p} without session`);
		assert.ok(r.text.includes('"unauthorized"'));
	}
	expectStatus(await raw("GET", "/accounts", undefined, `${SESSION_COOKIE}=forged`), 401);

	// Bad credentials, including an unknown user, get the same vague 401.
	for (const body of ['{"username":"tester","password":"wrong password!"}', '{"username":"nobody","password":"whatever12345"}']) {
		const r = await raw("POST", "/auth/login", body);
		assert.equal(r.status, 401);
		assert.ok(r.text.includes("invalid username or password"), r.text);
	}

	// Good login: hardened cookie, and the session works.
	let r = await raw("POST", "/auth/login", '{"username":"  Tester ","password":"correct horse battery"}');
	expectStatus(r, 200);
	const c = sessionFrom(r);
	assert.deepEqual(c.attrs, ["Path=/", "Max-Age=604800", "HttpOnly", "SameSite=Strict"]);
	const cookie = `${SESSION_COOKIE}=${c.value}`;
	r = await raw("GET", "/auth/me", undefined, cookie);
	expectStatus(r, 200);
	assert.ok(r.text.includes('"username":"tester"'), r.text);
	assert.ok(!r.text.includes('"id"'), "user id must not be exposed");

	// Secure flag when behind a TLS-terminating proxy.
	r = await request("POST", "/api/v1/auth/login", {
		rawBody: '{"username":"tester","password":"correct horse battery"}',
		headers: { "Content-Type": "application/json", "X-Forwarded-Proto": "https" },
	});
	const other = sessionFrom(r);
	assert.ok(other.attrs.includes("Secure"), "expected Secure cookie behind https proxy");
	const otherCookie = `${SESSION_COOKIE}=${other.value}`;

	// Audit trail: entries record who posted them.
	const post = (p: string, body: unknown) => request("POST", "/api/v1" + p, { body, cookie });
	expectStatus(await post("/accounts", { code: "1000", name: "Bank CAD", type: "asset", currency: "CAD" }), 201);
	expectStatus(await post("/accounts", { code: "4000", name: "Sales", type: "revenue", currency: "CAD" }), 201);
	r = await post("/journal-entries", entry("2026-10-01", "Sale", dr("1000", 100), cr("4000", 100)));
	expectStatus(r, 201);
	assert.equal(r.json().created_by, "tester");

	// Password change: validates, keeps this session, ends the others.
	expectStatus(await raw("POST", "/auth/password", '{"current_password":"nope","new_password":"short"}', cookie), 422);
	expectStatus(await raw("POST", "/auth/password", '{"current_password":"correct horse battery","new_password":"a much better passphrase"}', cookie), 204);
	expectStatus(await raw("GET", "/auth/me", undefined, cookie), 200);
	expectStatus(await raw("GET", "/auth/me", undefined, otherCookie), 401);

	// Logout ends the session server-side and clears the cookie.
	r = await raw("POST", "/auth/logout", undefined, cookie);
	expectStatus(r, 204);
	assert.equal(sessionFrom(r).header, `${SESSION_COOKIE}=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict`);
	expectStatus(await raw("GET", "/auth/me", undefined, cookie), 401);

	// Disabled users cannot log in and lose existing sessions.
	r = await raw("POST", "/auth/login", '{"username":"tester","password":"a much better passphrase"}');
	const c2 = `${SESSION_COOKIE}=${sessionFrom(r).value}`;
	await setUserActive(app, "tester", false);
	expectStatus(await raw("GET", "/auth/me", undefined, c2), 401);
	expectStatus(await raw("POST", "/auth/login", '{"username":"tester","password":"a much better passphrase"}'), 401);
	await setUserActive(app, "tester", true);

	// Users can't be deleted (audit trail), and passwords are not stored in clear.
	await assert.rejects(app.db.query(`DELETE FROM users`));
	const hash = (await app.db.query(`SELECT password_hash FROM users WHERE username = 'tester'`)).rows[0].password_hash as string;
	assert.match(hash, /^\$2[aby]\$12\$/);

	// Validation of new users.
	await assert.rejects(createUser(app, "Bad Name!", "long enough password"));
	await assert.rejects(createUser(app, "bob", "short"));

	// Rate limiting: after repeated failures even the right password is refused.
	for (let i = 0; i < LOGIN_MAX_FAILURES; i++) {
		await raw("POST", "/auth/login", '{"username":"tester","password":"wrong wrong wrong"}');
	}
	expectStatus(await raw("POST", "/auth/login", '{"username":"tester","password":"a much better passphrase"}'), 429);
});

test("hashes from the Go version keep working", async (t) => {
	const ta = await setupTestApp(t);
	if (!ta) return;
	// Written by golang.org/x/crypto/bcrypt (cost 12) for "correct horse battery".
	const goHash = "$2a$12$4nK2BHvfnf.Q0CwnKdjcPeBANQpCHG9oS0wNubF4F7BDtS2O9s/VG";
	await ta.app.db.query(`INSERT INTO users (username, password_hash) VALUES ('gouser', $1)`, [goHash]);
	const r = await ta.request("POST", "/api/v1/auth/login", { body: { username: "gouser", password: "correct horse battery" } });
	expectStatus(r, 200);
});

test("financial statements", async (t) => {
	const ta = await setupTestApp(t);
	if (!ta) return;
	const { call } = ta;
	await createAccount(ta, "1000", "Bank CAD", "asset", "CAD");
	await createAccount(ta, "1010", "Bank USD", "asset", "USD");
	await createAccount(ta, "1900", "FX clearing CAD", "asset", "CAD");
	await createAccount(ta, "1910", "FX clearing USD", "liability", "USD");
	await createAccount(ta, "2000", "Credit card", "liability", "CAD");
	await createAccount(ta, "3000", "Owner equity", "equity", "CAD");
	await createAccount(ta, "4000", "Sales", "revenue", "CAD");
	await createAccount(ta, "4010", "Sales USD", "revenue", "USD");
	await createAccount(ta, "6100", "Supplies", "expense", "CAD");

	const post = async (date: string, ...lines: Line[]) => expectStatus(await call("POST", "/journal-entries", entry(date, "x", ...lines)), 201);
	await post("2025-12-15", dr("1000", 1000000), cr("3000", 1000000)); // capital 10,000
	await post("2025-12-20", dr("1000", 50000), cr("4000", 50000)); // 2025 sale 500
	await post("2026-02-01", dr("1000", 120000), cr("4000", 120000)); // 2026 sale 1,200
	await post("2026-03-01", dr("6100", 30000), cr("2000", 30000)); // supplies on card 300
	await post("2026-04-01", dr("1010", 80000), cr("4010", 80000)); // USD sale 800
	await post("2026-05-01", dr("1900", 13700), cr("1000", 13700), dr("1910", 10000), cr("1010", 10000)); // swap

	let r = await call("GET", "/reports/balance-sheet?as_of=2026-12-31");
	expectStatus(r, 200);
	let bs = r.json();
	assert.equal(bs.currencies.length, 2);
	const [cad, usd] = bs.currencies;
	// CAD assets: bank 10000+500+1200-137 = 11563; clearing 137 => 11700.
	// Liabilities: card 300. Equity 10000 + net income (500+1200-300=1400) = 11400.
	assert.equal(cad.currency, "CAD");
	assert.equal(cad.total_assets_cents, 1170000);
	assert.equal(cad.total_liabilities_cents, 30000);
	assert.equal(cad.net_income_cents, 140000);
	assert.equal(cad.total_equity_cents, 1140000);
	assert.equal(cad.balanced, true);
	// USD: bank 800-100 = 700; clearing (a liability here) shows -100; equity = net income 800.
	assert.equal(usd.total_assets_cents, 70000);
	assert.equal(usd.total_liabilities_cents, -10000);
	assert.equal(usd.total_equity_cents, 80000);
	assert.equal(usd.balanced, true);

	// Balance sheet as of an earlier date.
	r = await call("GET", "/reports/balance-sheet?as_of=2025-12-31&currency=CAD");
	bs = r.json();
	assert.equal(bs.currencies.length, 1);
	assert.equal(bs.currencies[0].total_assets_cents, 1050000);
	assert.equal(bs.currencies[0].net_income_cents, 50000);

	// Income statement for 2026 only.
	r = await call("GET", "/reports/income-statement?from=2026-01-01&to=2026-12-31");
	expectStatus(r, 200);
	const is = r.json();
	assert.equal(is.currencies.length, 2);
	const c = is.currencies[0];
	assert.equal(c.total_revenue_cents, 120000);
	assert.equal(c.total_expenses_cents, 30000);
	assert.equal(c.net_income_cents, 90000);
	assert.equal(c.revenue.length, 1);
	assert.equal(c.revenue[0].account_code, "4000");
	assert.equal(c.expenses.length, 1);
	assert.equal(is.currencies[1].currency, "USD");
	assert.equal(is.currencies[1].net_income_cents, 80000);
	expectStatus(await call("GET", "/reports/income-statement?from=2026-02-01&to=2026-01-01"), 422);
});
