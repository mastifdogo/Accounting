import { test } from "node:test";
import assert from "node:assert/strict";
import { formatCents, parseCents, MAX_CENTS } from "../src/money.ts";
import { csvText } from "../src/files.ts";
import { validateJournalEntry } from "../src/ledger.ts";
import { parseDate } from "../src/date.ts";
import { parseImportCSV } from "../src/import.ts";
import { csvRecord, readCsv } from "../src/csv.ts";
import { DecodeError, decodeJournalEntryCreate, parseJson } from "../src/decode.ts";
import { formDataBoundary, parseMultipart } from "../src/multipart.ts";
import { toJson } from "../src/http.ts";
import type { JournalEntryCreate, JournalLineCreate } from "../src/types.ts";

test("formatCents", () => {
	const cases: [bigint, string][] = [
		[0n, "0.00"],
		[5n, "0.05"],
		[100n, "1.00"],
		[123456n, "1234.56"],
		[-1n, "-0.01"],
		[-123456n, "-1234.56"],
		[2n ** 63n - 1n, "92233720368547758.07"],
		[-(2n ** 63n), "-92233720368547758.08"],
	];
	for (const [input, want] of cases) assert.equal(formatCents(input), want);
});

test("csvText neutralises formulas", () => {
	const cases: Record<string, string> = {
		Cash: "Cash",
		"=SUM(A1:A9)": "'=SUM(A1:A9)",
		"+1": "'+1",
		"-cmd": "'-cmd",
		"@x": "'@x",
		"": "",
		"Ünïcødé €": "Ünïcødé €",
		"\tleading tab": "'\tleading tab",
	};
	for (const [input, want] of Object.entries(cases)) assert.equal(csvText(input), want);
});

const line = (code: string, debit: bigint, credit: bigint, memo = ""): JournalLineCreate => ({
	account_code: code,
	debit_cents: debit,
	credit_cents: credit,
	memo,
});
const dr = (code: string, cents: bigint) => line(code, cents, 0n);
const cr = (code: string, cents: bigint) => line(code, 0n, cents);

test("validateJournalEntry", async (t) => {
	const valid = (): JournalEntryCreate => ({
		entry_date: "2026-10-01",
		description: "Sale",
		reference: "",
		lines: [dr("1000", 10000n), cr("4000", 10000n)],
	});
	const cases: [string, (e: JournalEntryCreate) => void, string][] = [
		["valid", () => {}, ""],
		["split credit", (e) => (e.lines = [dr("1000", 100n), cr("4000", 60n), cr("6100", 40n)]), ""],
		["missing date", (e) => (e.entry_date = ""), "entry_date: is required"],
		["year out of range", (e) => (e.entry_date = "1899-12-31"), "year must be between"],
		["blank description", (e) => (e.description = "   "), "description: is required"],
		["description too long", (e) => (e.description = "x".repeat(501)), "at most 500"],
		["NUL in memo", (e) => (e.lines[0]!.memo = "a\0b"), "NUL"],
		["lone surrogate", (e) => (e.lines[0]!.memo = "a\ud800b"), "valid UTF-8"],
		["one line", (e) => (e.lines = e.lines.slice(0, 1)), "at least 2 lines"],
		["no lines", (e) => (e.lines = []), "at least 2 lines"],
		["too many lines", (e) => (e.lines = Array.from({ length: 1001 }, () => line("", 0n, 0n))), "at most 1000"],
		["unbalanced", (e) => (e.lines[1]!.credit_cents = 9999n), "unbalanced"],
		["both sides", (e) => (e.lines[0]!.credit_cents = 1n), "not both"],
		["zero line", (e) => e.lines.push(line("6100", 0n, 0n)), "non-zero"],
		["negative", (e) => ((e.lines[0]!.debit_cents = -10000n), (e.lines[1]!.credit_cents = -10000n)), "must not be negative"],
		["missing account", (e) => (e.lines[0]!.account_code = ""), "lines[0].account_code: is required"],
		["bad account code", (e) => (e.lines[0]!.account_code = "../x"), "not a valid account code"],
		["amount too large", (e) => ((e.lines[0]!.debit_cents = MAX_CENTS + 1n), (e.lines[1]!.credit_cents = MAX_CENTS + 1n)), "exceeds the maximum"],
		[
			"total too large",
			(e) => (e.lines = [dr("1000", MAX_CENTS), dr("1000", MAX_CENTS), cr("4000", MAX_CENTS), cr("4000", MAX_CENTS)]),
			"entry total exceeds",
		],
	];
	for (const [name, mutate, wantErr] of cases) {
		await t.test(name, () => {
			const e = valid();
			mutate(e);
			if (wantErr === "") assert.doesNotThrow(() => validateJournalEntry(e));
			else assert.throws(() => validateJournalEntry(e), (err: Error) => err.message.includes(wantErr), `expected error containing ${wantErr}`);
		});
	}
});

test("parseDate", () => {
	assert.throws(() => parseDate("2026-02-30"));
	assert.throws(() => parseDate("2026-2-28"));
	assert.throws(() => parseDate("2026-13-01"));
	assert.equal(parseDate("2024-02-29"), "2024-02-29");
	assert.throws(() => parseDate("2100-02-29"));
	assert.equal(parseDate("2026-02-28"), "2026-02-28");
});

test("parseCents", () => {
	const ok: [string, bigint][] = [
		["", 0n],
		["0", 0n],
		["1", 100n],
		["1.5", 150n],
		["1.05", 105n],
		[" 1234.56 ", 123456n],
		["0.01", 1n],
		["90071992547409.91", MAX_CENTS],
	];
	for (const [input, want] of ok) assert.equal(parseCents(input), want, input);
	for (const input of ["1.234", "-1", "+1", "1,000.00", "$5", "1e3", ".5", "5.", "abc", "1.2.3", "90071992547409.92", "99999999999999999999"]) {
		assert.throws(() => parseCents(input), Error, input);
	}
});

test("parseImportCSV", () => {
	const good =
		"\ufeffEntry_Key,entry_date,description,reference,account_code,debit,credit,memo,currency\n" +
		"A,2026-10-01,Sale,INV-1,1000,100.00,,,CAD\n" +
		'A,,,,4000,,100,"memo, with comma",\n' +
		"B,2026-10-02,FX,,1000,13.70,,,\n" +
		"B,2026-10-02,FX,,1900,,13.70,,\n" +
		"B,2026-10-02,FX,,1910,10.00,,,usd\n" +
		"B,2026-10-02,FX,,1010,,10.00,,\n";
	const groups = parseImportCSV(Buffer.from(good));
	assert.equal(groups.length, 2);
	assert.equal(groups[0]!.entry.lines.length, 2);
	assert.equal(groups[1]!.entry.lines.length, 4);
	const a = groups[0]!;
	assert.equal(a.entry.reference, "INV-1");
	assert.equal(a.entry.lines[1]!.credit_cents, 10000n);
	assert.equal(a.entry.lines[1]!.memo, "memo, with comma");
	assert.equal(a.rows[1], 3);
	assert.equal(groups[1]!.currency[2], "USD");

	const bad: Record<string, string | Buffer> = {
		"missing column": "entry_key,entry_date,description,account_code,debit\nA,2026-10-01,x,1000,1\n",
		"unknown column": "entry_key,entry_date,description,account_code,debit,credit,amount\n",
		"duplicate col": "entry_key,entry_date,description,account_code,debit,credit,debit\n",
		empty: "",
		"no entries": "entry_key,entry_date,description,account_code,debit,credit\n",
		unbalanced: "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1.00,\nA,,,4000,,0.99\n",
		"single line": "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1.00,\n",
		"bad amount": "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1.001,\nA,,,4000,,1.001\n",
		"bad date": "entry_key,entry_date,description,account_code,debit,credit\nA,01/10/2026,x,1000,1,\nA,,,4000,,1\n",
		conflicting: "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1,\nA,2026-10-02,,4000,,1\n",
		"non-contiguous": "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1,\nB,2026-10-01,y,1000,1,\nA,,,4000,,1\n",
		"bad currency": "entry_key,entry_date,description,account_code,debit,credit,currency\nA,2026-10-01,x,1000,1,,EUR\nA,,,4000,,1,\n",
		"ragged row": "entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,x,1000,1\n",
		"invalid utf8": Buffer.concat([Buffer.from("entry_key,entry_date,description,account_code,debit,credit\nA,2026-10-01,"), Buffer.from([0xff]), Buffer.from(",1000,1,\n")]),
	};
	for (const [name, input] of Object.entries(bad)) {
		assert.throws(() => parseImportCSV(Buffer.isBuffer(input) ? input : Buffer.from(input)), Error, name);
	}
});

test("readCsv follows encoding/csv", () => {
	const rows = (s: string) => [...readCsv(s)].map((r) => [r.line, ...r.fields]);
	assert.deepEqual(rows('a,b\r\n\r\n"x\r\ny",""""\n"",\n'), [
		[1, "a", "b"],
		[3, "x\ny", '"'],
		[5, "", ""],
	]);
	assert.deepEqual(rows("a,b\rc"), [[1, "a", "b\rc"]]);
	assert.throws(() => rows('a,b\nx,y"z\n'), /parse error on line 2, column 4: bare " in non-quoted-field/);
	// Expected messages are what Go's encoding/csv reports for the same input.
	assert.throws(() => rows('a,b\n"x"y,z\n'), /^Error: parse error on line 2, column 3: extraneous or missing " in quoted-field$/);
	assert.throws(() => rows('a,b\n"x\n'), /^Error: parse error on line 2, column 4: extraneous or missing " in quoted-field$/);
	assert.throws(() => rows('a,b\n"x\nyy"z,w\n'), /^Error: record on line 2; parse error on line 3, column 3: extraneous or missing " in quoted-field$/);
	assert.throws(() => rows('a,b\nüü,y"z\n'), /column 7: bare "/);
	assert.throws(() => rows("a,b\nc\n"), /record on line 2: wrong number of fields/);
});

test("csvRecord quotes like encoding/csv", () => {
	const fields = ["a", "", "b,c", 'q"', " lead", "x\ny", "\\.", "\u00a0nb", "r\rx"];
	assert.equal(csvRecord(fields), 'a,,"b,c","q"""," lead","x\ny","\\.","\u00a0nb","r\rx"\n');
});

test("decode rejects floats, unknown fields and wrong types", () => {
	const ok = decodeJournalEntryCreate(
		parseJson('{"entry_date":"2026-10-01","description":"x","lines":[{"account_code":"1","debit_cents":9007199254740993,"credit_cents":0}]}'),
	);
	assert.equal(ok.lines[0]!.debit_cents, 9007199254740993n); // exact, not rounded
	for (const body of [
		'{"lines":[{"account_code":"1","debit_cents":1.5}]}',
		'{"lines":[{"account_code":"1","debit_cents":1.0}]}',
		'{"lines":[{"account_code":"1","debit_cents":1e2}]}',
		'{"lines":[{"account_code":"1","debit_cents":"5"}]}',
		'{"lines":[{"account_id":1}]}',
		'{"status":"draft"}',
		'{"entry_date":"2026-02-30"}',
		'{"lines":{}}',
		"[]",
	]) {
		assert.throws(() => decodeJournalEntryCreate(parseJson(body)), DecodeError, body);
	}
});

test("toJson writes bigint exactly", () => {
	assert.equal(toJson({ a: 2n ** 63n - 1n, b: [1n], c: new Date(0) }), '{"a":9223372036854775807,"b":[1],"c":"1970-01-01T00:00:00.000Z"}');
});

test("parseMultipart", () => {
	const ct = "multipart/form-data; boundary=----X";
	const b = formDataBoundary(ct)!;
	const body = Buffer.from(
		"------X\r\n" +
			'Content-Disposition: form-data; name="note"\r\n\r\nhello\r\n' +
			"------X\r\n" +
			'Content-Disposition: form-data; name="file"; filename="../../a b.csv"\r\nContent-Type: text/csv\r\n\r\n' +
			"x,y\r\n1,2\r\n" +
			"\r\n------X--\r\n",
	);
	const parts = parseMultipart(body, b);
	assert.equal(parts.length, 2);
	assert.equal(parts[1]!.name, "file");
	assert.equal(parts[1]!.filename, "a b.csv");
	assert.equal(parts[1]!.data.toString(), "x,y\r\n1,2\r\n");
	assert.equal(formDataBoundary("text/plain"), null);
	assert.equal(formDataBoundary("multipart/form-data"), null);
});
