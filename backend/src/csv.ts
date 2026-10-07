// RFC 4180 CSV with the exact rules of the previous Go implementation
// (encoding/csv): "\n" line endings on output, "\r\n" accepted on input,
// blank lines skipped, every record as wide as the first, and strict
// quoting (a bare `"` inside an unquoted field is an error).

import { startsWithSpace } from "./text.ts";

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

function fieldNeedsQuotes(field: string): boolean {
	if (field === "") return false;
	if (field === "\\.") return true;
	return /[\n\r",]/.test(field) || startsWithSpace(field);
}

/** Encodes one record, including its trailing "\n". */
export function csvRecord(fields: string[]): string {
	return (
		fields
			.map((f) => (fieldNeedsQuotes(f) ? `"${f.replaceAll('"', '""')}"` : f))
			.join(",") + "\n"
	);
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

export class CsvParseError extends Error {
	readonly startLine: number;
	readonly line: number;
	readonly column: number;

	constructor(startLine: number, line: number, column: number, reason: string) {
		let msg: string;
		if (reason === FIELD_COUNT) msg = `record on line ${startLine}: ${reason}`;
		else if (startLine !== line) msg = `record on line ${startLine}; parse error on line ${line}, column ${column}: ${reason}`;
		else msg = `parse error on line ${line}, column ${column}: ${reason}`;
		super(msg);
		this.startLine = startLine;
		this.line = line;
		this.column = column;
	}
}

const BARE_QUOTE = 'bare " in non-quoted-field';
const QUOTE = 'extraneous or missing " in quoted-field';
const FIELD_COUNT = "wrong number of fields";

export interface CsvRow {
	fields: string[];
	/** 1-based line number where the record starts. */
	line: number;
}

const bytes = (s: string) => Buffer.byteLength(s);

/**
 * Yields the records of a CSV document. Throws CsvParseError (after yielding
 * every earlier record) at the first malformed record. Every record must
 * have as many fields as the first one.
 */
export function* readCsv(text: string): Generator<CsvRow> {
	let offset = 0;
	let numLine = 0;
	let eof = false;

	// One physical line including its "\n" ("\r\n" normalised), or "" at EOF.
	const readLine = (): string => {
		if (offset >= text.length) {
			numLine++;
			eof = true;
			return "";
		}
		const nl = text.indexOf("\n", offset);
		let line: string;
		if (nl < 0) {
			line = text.slice(offset);
			offset = text.length;
			if (line.endsWith("\r")) line = line.slice(0, -1); // trailing \r before EOF
		} else {
			line = text.slice(offset, nl + 1);
			offset = nl + 1;
		}
		numLine++;
		if (line.endsWith("\r\n")) line = line.slice(0, -2) + "\n";
		return line;
	};
	const lengthNL = (s: string) => (s.endsWith("\n") ? 1 : 0);

	let width = -1;
	for (;;) {
		let line = "";
		for (;;) {
			line = readLine();
			if (eof) return;
			if (line.length === lengthNL(line)) continue; // skip empty lines
			break;
		}
		const recLine = numLine;
		let posLine = numLine;
		let col = 1;
		const fields: string[] = [];

		parse: for (;;) {
			if (line.length === 0 || line[0] !== '"') {
				// Unquoted field.
				const i = line.indexOf(",");
				const field = i >= 0 ? line.slice(0, i) : line.slice(0, line.length - lengthNL(line));
				const j = field.indexOf('"');
				if (j >= 0) throw new CsvParseError(recLine, numLine, col + bytes(field.slice(0, j)), BARE_QUOTE);
				fields.push(field);
				if (i >= 0) {
					col += bytes(line.slice(0, i)) + 1;
					line = line.slice(i + 1);
					continue parse;
				}
				break parse;
			}
			// Quoted field.
			let field = "";
			line = line.slice(1);
			col += 1;
			for (;;) {
				const i = line.indexOf('"');
				if (i >= 0) {
					field += line.slice(0, i);
					col += bytes(line.slice(0, i)) + 1;
					line = line.slice(i + 1);
					if (line[0] === '"') {
						field += '"'; // `""` sequence
						line = line.slice(1);
						col += 1;
					} else if (line[0] === ",") {
						line = line.slice(1); // `",` ends the field
						col += 1;
						fields.push(field);
						continue parse;
					} else if (line.length === lengthNL(line)) {
						fields.push(field); // `"\n` ends the record
						break parse;
					} else {
						throw new CsvParseError(recLine, numLine, col - 1, QUOTE);
					}
				} else if (line.length > 0) {
					// The quoted field continues on the next line.
					field += line;
					col += bytes(line);
					line = readLine();
					eof = false; // an unterminated quote is reported below
					if (line.length > 0) {
						posLine++;
						col = 1;
					}
				} else {
					throw new CsvParseError(recLine, posLine, col, QUOTE);
				}
			}
		}

		if (width < 0) width = fields.length;
		else if (fields.length !== width) throw new CsvParseError(recLine, recLine, 1, FIELD_COUNT);
		yield { fields, line: recLine };
	}
}
