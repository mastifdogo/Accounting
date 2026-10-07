// Strict JSON request decoding. The OpenAPI schemas set
// additionalProperties: false, so unknown fields are rejected, and integer
// fields must be written as integers ("1.0" and "1e2" are refused) and are
// parsed exactly from their source text.

import { parseDate } from "./date.ts";
import type { AccountCreate, AccountUpdate, JournalEntryCreate, JournalLineCreate, ReverseRequest } from "./types.ts";

/** A malformed request body (HTTP 400 "invalid JSON: ..."). */
export class DecodeError extends Error {}

/** A JSON number, kept as its source text. */
class JsonNumber {
	readonly source: string;
	constructor(source: string) {
		this.source = source;
	}
}

type Reviver = (this: unknown, key: string, value: unknown, context: { source?: string }) => unknown;

export function parseJson(text: string): unknown {
	const reviver: Reviver = (_key, value, context) => (typeof value === "number" ? new JsonNumber(context.source ?? String(value)) : value);
	try {
		return JSON.parse(text, reviver as (key: string, value: unknown) => unknown);
	} catch (err) {
		throw new DecodeError((err as Error).message);
	}
}

function kind(v: unknown): string {
	if (v instanceof JsonNumber) return "number";
	if (Array.isArray(v)) return "array";
	return typeof v;
}

function mismatch(v: unknown, path: string, want: string): DecodeError {
	return new DecodeError(`json: cannot unmarshal ${kind(v)} into ${path} of type ${want}`);
}

/** An object with only the allowed keys. JSON null decodes as an empty object. */
function object(v: unknown, path: string, keys: readonly string[]): Record<string, unknown> {
	if (v === null) return {};
	if (typeof v !== "object" || Array.isArray(v) || v instanceof JsonNumber) throw mismatch(v, path, "object");
	for (const k of Object.keys(v)) {
		if (!keys.includes(k)) throw new DecodeError(`json: unknown field ${JSON.stringify(k)}`);
	}
	return v as Record<string, unknown>;
}

/** Field accessors: undefined when absent or null. */
function str(o: Record<string, unknown>, key: string, path: string): string | undefined {
	const v = o[key];
	if (v === undefined || v === null) return undefined;
	if (typeof v !== "string") throw mismatch(v, `${path}${key}`, "string");
	return v;
}

function bool(o: Record<string, unknown>, key: string, path: string): boolean | undefined {
	const v = o[key];
	if (v === undefined || v === null) return undefined;
	if (typeof v !== "boolean") throw mismatch(v, `${path}${key}`, "bool");
	return v;
}

const INT64_MAX = 2n ** 63n - 1n;
const INT64_MIN = -(2n ** 63n);

function int64(o: Record<string, unknown>, key: string, path: string): bigint | undefined {
	const v = o[key];
	if (v === undefined || v === null) return undefined;
	if (!(v instanceof JsonNumber)) throw mismatch(v, `${path}${key}`, "int64");
	if (!/^-?(0|[1-9][0-9]*)$/.test(v.source)) throw new DecodeError(`json: cannot unmarshal number ${v.source} into ${path}${key} of type int64`);
	const n = BigInt(v.source);
	if (n > INT64_MAX || n < INT64_MIN) throw new DecodeError(`json: cannot unmarshal number ${v.source} into ${path}${key} of type int64`);
	return n;
}

function date(o: Record<string, unknown>, key: string): string | undefined {
	const v = o[key];
	if (v === undefined || v === null) return undefined;
	if (typeof v !== "string") throw new DecodeError("date must be a string in YYYY-MM-DD format");
	try {
		return parseDate(v);
	} catch (err) {
		throw new DecodeError((err as Error).message, { cause: err });
	}
}

// ---------------------------------------------------------------------------
// Request bodies
// ---------------------------------------------------------------------------

export function decodeAccountCreate(v: unknown): AccountCreate {
	const o = object(v, "AccountCreate", ["code", "name", "type", "currency", "description"]);
	const p = "AccountCreate.";
	return {
		code: str(o, "code", p) ?? "",
		name: str(o, "name", p) ?? "",
		type: str(o, "type", p) ?? "",
		currency: str(o, "currency", p) ?? "",
		description: str(o, "description", p) ?? "",
	};
}

export function decodeAccountUpdate(v: unknown): AccountUpdate {
	const o = object(v, "AccountUpdate", ["name", "description", "is_active"]);
	const p = "AccountUpdate.";
	const out: AccountUpdate = {};
	const name = str(o, "name", p);
	const description = str(o, "description", p);
	const isActive = bool(o, "is_active", p);
	if (name !== undefined) out.name = name;
	if (description !== undefined) out.description = description;
	if (isActive !== undefined) out.is_active = isActive;
	return out;
}

function decodeLine(v: unknown, i: number): JournalLineCreate {
	const p = `lines[${i}].`;
	const o = object(v, `lines[${i}]`, ["account_code", "debit_cents", "credit_cents", "memo"]);
	return {
		account_code: str(o, "account_code", p) ?? "",
		debit_cents: int64(o, "debit_cents", p) ?? 0n,
		credit_cents: int64(o, "credit_cents", p) ?? 0n,
		memo: str(o, "memo", p) ?? "",
	};
}

export function decodeJournalEntryCreate(v: unknown): JournalEntryCreate {
	const o = object(v, "JournalEntryCreate", ["entry_date", "description", "reference", "lines"]);
	const p = "JournalEntryCreate.";
	const lines = o.lines;
	if (lines !== undefined && lines !== null && !Array.isArray(lines)) throw mismatch(lines, `${p}lines`, "array");
	return {
		entry_date: date(o, "entry_date") ?? "",
		description: str(o, "description", p) ?? "",
		reference: str(o, "reference", p) ?? "",
		lines: ((lines as unknown[] | null | undefined) ?? []).map(decodeLine),
	};
}

export function decodeReverseRequest(v: unknown): ReverseRequest {
	const o = object(v, "ReverseRequest", ["entry_date", "description"]);
	const p = "ReverseRequest.";
	const out: ReverseRequest = {};
	const d = date(o, "entry_date");
	const description = str(o, "description", p);
	if (d !== undefined) out.entry_date = d;
	if (description !== undefined) out.description = description;
	return out;
}

export function decodeExportRequest(v: unknown): { from: string | null; to: string | null; currency: string | null } {
	const o = object(v, "ExportRequest", ["from", "to", "currency"]);
	const p = "ExportRequest.";
	return { from: date(o, "from") ?? null, to: date(o, "to") ?? null, currency: str(o, "currency", p) ?? null };
}

export function decodeImportRequest(v: unknown): { filename: string } {
	const o = object(v, "ImportRequest", ["filename"]);
	return { filename: str(o, "filename", "ImportRequest.") ?? "" };
}

export function decodeLoginRequest(v: unknown): { username: string; password: string } {
	const o = object(v, "LoginRequest", ["username", "password"]);
	const p = "LoginRequest.";
	return { username: str(o, "username", p) ?? "", password: str(o, "password", p) ?? "" };
}

export function decodePasswordChange(v: unknown): { current_password: string; new_password: string } {
	const o = object(v, "PasswordChange", ["current_password", "new_password"]);
	const p = "PasswordChange.";
	return { current_password: str(o, "current_password", p) ?? "", new_password: str(o, "new_password", p) ?? "" };
}
