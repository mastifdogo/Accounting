// HTTP layer. Paths match api/openapi.yaml under /api/v1; everything else
// serves the built SvelteKit frontend.

import type { IncomingMessage, ServerResponse } from "node:http";
import { randomBytes } from "node:crypto";
import type { App } from "./app.ts";
import { isPgError } from "./db.ts";
import {
	BadCredentialsError,
	ConflictError,
	NotFoundError,
	TooLargeError,
	TooManyLoginsError,
	UnauthorizedError,
	ValidationError,
	type FieldError,
} from "./errors.ts";
import { parseDate } from "./date.ts";
import { log } from "./log.ts";
import { ACCOUNT_CODE_RE, isCurrency, publicUser, type Currency, type User } from "./types.ts";
import {
	DecodeError,
	decodeAccountCreate,
	decodeAccountUpdate,
	decodeExportRequest,
	decodeImportRequest,
	decodeJournalEntryCreate,
	decodeLoginRequest,
	decodePasswordChange,
	decodeReverseRequest,
	parseJson,
} from "./decode.ts";
import { createAccount, getAccount, getAccountLedger, getTrialBalance, listAccounts, listCurrencies, updateAccount } from "./accounts.ts";
import { createJournalEntry, getJournalEntry, listJournalEntries, reverseJournalEntry, type EntryFilter } from "./ledger.ts";
import { getBalanceSheet, getIncomeStatement } from "./reports.ts";
import { CSV_FILENAME_RE, MAX_FILE_BYTES, exportGeneralLedger, listCsvFiles, openCsvFile, saveUploadedCsv } from "./files.ts";
import { importJournalEntries } from "./import.ts";
import { changePassword, login, logout, normalizeUsername, sessionUser, SESSION_COOKIE, SESSION_MAX_AGE_SECONDS } from "./auth.ts";
import { MultipartError, formDataBoundary, parseHeaderParams, parseMultipart } from "./multipart.ts";
import { staticHandler } from "./static.ts";

const MAX_BODY_BYTES = 1 << 20; // 1 MiB for JSON
const MULTIPART_OVERHEAD = 64 << 10;

/** An error that maps directly to a status and error code. */
class HttpError extends Error {
	readonly status: number;
	readonly code: string;
	constructor(status: number, code: string, message: string) {
		super(message);
		this.status = status;
		this.code = code;
	}
}

class BodyTooLargeError extends Error {}

interface Ctx {
	app: App;
	req: IncomingMessage;
	res: ServerResponse;
	params: Record<string, string>;
	query: URLSearchParams;
	user: User | null;
	token: string;
}

type Handler = (c: Ctx) => Promise<void>;

interface Route {
	method: string;
	segments: string[];
	auth: boolean;
	handler: Handler;
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

/** JSON.stringify that writes bigint values as exact JSON numbers. */
export function toJson(v: unknown): string {
	const rawJSON = (JSON as unknown as { rawJSON(text: string): unknown }).rawJSON;
	return JSON.stringify(v, (_k, x) => (typeof x === "bigint" ? rawJSON(x.toString()) : x));
}

function writeJSON(res: ServerResponse, status: number, v: unknown): void {
	const body = toJson(v) + "\n";
	res.writeHead(status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) });
	res.end(body);
}

function writeError(res: ServerResponse, status: number, code: string, message: string, details?: FieldError[]): void {
	if (res.headersSent) {
		res.destroy();
		return;
	}
	const error: { code: string; message: string; details?: FieldError[] } = { code, message };
	if (details && details.length > 0) error.details = details;
	writeJSON(res, status, { error });
}

function findCause<T>(err: unknown, test: (e: unknown) => e is T): T | undefined {
	for (let e = err, depth = 0; e && depth < 10; e = (e as Error).cause, depth++) {
		if (test(e)) return e;
	}
	return undefined;
}

const UNIQUE_MESSAGES: Record<string, string> = {
	accounts_code_unique: "an account with this code already exists",
	journal_entries_reversed_once: "this journal entry has already been reversed",
	csv_imports_sha256_unique: "this file's content has already been imported",
};

/**
 * Maps ledger and PostgreSQL errors to API error responses. Database
 * constraint violations (the last line of defence) surface as 409/422 with
 * the trigger's message rather than as opaque 500s.
 */
function writeErr(c: Ctx, err: unknown): void {
	const { res } = c;
	if (err instanceof ValidationError) return writeError(res, 422, "validation_failed", "request validation failed", err.details);
	if (err instanceof HttpError) return writeError(res, err.status, err.code, err.message);
	if (err instanceof UnauthorizedError) return writeError(res, 401, "unauthorized", "login required");
	if (err instanceof BadCredentialsError) return writeError(res, 401, "unauthorized", err.message);
	if (err instanceof TooManyLoginsError) return writeError(res, 429, "too_many_requests", err.message);
	if (err instanceof TooLargeError) return writeError(res, 413, "payload_too_large", `file is larger than ${MAX_FILE_BYTES} bytes`);
	if (err instanceof NotFoundError) return writeError(res, 404, "not_found", "not found");
	if (err instanceof ConflictError) return writeError(res, 409, "conflict", err.message);
	const pg = findCause(err, isPgError);
	if (pg) {
		const code = pg.code ?? "";
		if (code === "23505") return writeError(res, 409, "conflict", UNIQUE_MESSAGES[pg.constraint ?? ""] ?? pg.message);
		if (code === "23001") return writeError(res, 409, "conflict", pg.message); // immutability triggers
		if (code.startsWith("23") || code.startsWith("22")) return writeError(res, 422, "validation_failed", pg.message);
		if (code === "57014") return writeError(res, 503, "internal_error", "request timed out or was cancelled");
	}
	log.error("internal error", { err: err instanceof Error ? err.message : String(err), path: c.req.url?.split("?")[0] });
	writeError(res, 500, "internal_error", "internal server error");
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

/** Reads the whole body, or throws BodyTooLargeError past limit bytes. */
function readBody(req: IncomingMessage, limit: number): Promise<Buffer> {
	return new Promise((resolve, reject) => {
		const declared = Number(req.headers["content-length"]);
		if (declared > limit) {
			req.resume();
			reject(new BodyTooLargeError());
			return;
		}
		const chunks: Buffer[] = [];
		let size = 0;
		const onData = (chunk: Buffer) => {
			size += chunk.length;
			if (size > limit) {
				req.off("data", onData);
				req.resume(); // discard the rest
				reject(new BodyTooLargeError());
				return;
			}
			chunks.push(chunk);
		};
		req.on("data", onData);
		req.on("end", () => resolve(Buffer.concat(chunks)));
		req.on("error", reject);
	});
}

/**
 * Reads a JSON body. An empty body yields undefined unless required. A
 * non-empty body must be sent as application/json, which browsers cannot do
 * cross-origin without a CORS preflight.
 */
async function readJSON(c: Ctx, required: boolean): Promise<unknown> {
	const ct = c.req.headers["content-type"] ?? "";
	const length = Number(c.req.headers["content-length"] ?? 0);
	if ((ct !== "" || length > 0) && parseHeaderParams(ct).type !== "application/json") {
		throw new HttpError(415, "bad_request", "Content-Type must be application/json");
	}
	let body: Buffer;
	try {
		body = await readBody(c.req, MAX_BODY_BYTES);
	} catch (err) {
		if (err instanceof BodyTooLargeError) {
			c.res.setHeader("Connection", "close");
			throw new HttpError(413, "payload_too_large", "request body too large");
		}
		throw err;
	}
	const text = body.toString("utf8");
	if (text.trim() === "") {
		if (!required) return undefined;
		throw new HttpError(400, "bad_request", "request body is required");
	}
	return parseJson(text);
}

async function decodeBody<T>(c: Ctx, required: boolean, decode: (v: unknown) => T): Promise<T> {
	try {
		const v = await readJSON(c, required);
		return decode(v === undefined ? null : v);
	} catch (err) {
		if (err instanceof DecodeError) throw new HttpError(400, "bad_request", "invalid JSON: " + err.message);
		throw err;
	}
}

function queryBool(q: URLSearchParams, name: string): boolean {
	const s = q.get(name) ?? "";
	if (s === "") return false;
	if (["1", "t", "T", "TRUE", "true", "True"].includes(s)) return true;
	if (["0", "f", "F", "FALSE", "false", "False"].includes(s)) return false;
	const v = new ValidationError();
	v.add(name, "must be true or false");
	throw v;
}

function queryDate(q: URLSearchParams, name: string): string | null {
	const s = q.get(name) ?? "";
	if (s === "") return null;
	try {
		return parseDate(s);
	} catch (err) {
		const v = new ValidationError();
		v.add(name, (err as Error).message);
		throw v;
	}
}

function queryCurrency(q: URLSearchParams): Currency | null {
	const s = q.get("currency") ?? "";
	if (s === "") return null;
	const c = s.toUpperCase();
	if (!isCurrency(c)) {
		const v = new ValidationError();
		v.add("currency", "must be CAD or USD");
		throw v;
	}
	return c;
}

function queryDateRange(q: URLSearchParams): [from: string | null, to: string | null] {
	return [queryDate(q, "from"), queryDate(q, "to")];
}

/** A positive int64 path id; anything else is a 404. */
function pathId(c: Ctx, name: string): bigint {
	const s = c.params[name] ?? "";
	if (/^[+-]?[0-9]+$/.test(s)) {
		const id = BigInt(s);
		if (id >= 1n && id <= 2n ** 63n - 1n) return id;
	}
	throw new NotFoundError();
}

/** Parses an integer query parameter like Go's strconv.Atoi. */
function atoi(s: string): number | null {
	if (!/^[+-]?[0-9]+$/.test(s)) return null;
	const n = Number(s);
	return Number.isSafeInteger(n) ? n : null;
}

// ---------------------------------------------------------------------------
// Sessions and cookies
// ---------------------------------------------------------------------------

function sessionToken(req: IncomingMessage): string {
	for (const part of (req.headers.cookie ?? "").split(";")) {
		const eq = part.indexOf("=");
		if (eq < 0 || part.slice(0, eq).trim() !== SESSION_COOKIE) continue;
		let v = part.slice(eq + 1).trim();
		if (v.length >= 2 && v.startsWith('"') && v.endsWith('"')) v = v.slice(1, -1);
		return v;
	}
	return "";
}

/**
 * Whether the session cookie gets the Secure flag: "true"/"false" force it;
 * "auto" sets it when the request arrived via a reverse proxy that sets
 * X-Forwarded-Proto: https (the server itself speaks plain HTTP).
 */
function secureCookie(c: Ctx): boolean {
	if (c.app.cookieSecure === "true") return true;
	if (c.app.cookieSecure === "false") return false;
	const proto = c.req.headers["x-forwarded-proto"];
	return typeof proto === "string" && proto.toLowerCase() === "https";
}

function setSessionCookie(c: Ctx, token: string, maxAge: number): void {
	const attrs = [`${SESSION_COOKIE}=${token}`, "Path=/", `Max-Age=${Math.max(maxAge, 0)}`, "HttpOnly"];
	if (secureCookie(c)) attrs.push("Secure");
	attrs.push("SameSite=Strict");
	c.res.setHeader("Set-Cookie", attrs.join("; "));
}

function clientIP(req: IncomingMessage): string {
	return req.socket.remoteAddress ?? "";
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

async function handleHealth(c: Ctx): Promise<void> {
	let timer: NodeJS.Timeout | undefined;
	const timeout = new Promise<never>((_, reject) => {
		timer = setTimeout(() => reject(new Error("timeout")), 3000);
	});
	try {
		await Promise.race([c.app.db.query("SELECT 1"), timeout]);
		writeJSON(c.res, 200, { status: "ok", database: "ok" });
	} catch {
		writeJSON(c.res, 503, { status: "degraded", database: "unreachable" });
	} finally {
		clearTimeout(timer);
	}
}

async function handleLogin(c: Ctx): Promise<void> {
	const input = await decodeBody(c, true, decodeLoginRequest);
	const key = clientIP(c.req) + "|" + normalizeUsername(input.username);
	if (!c.app.logins.allowed(key)) throw new TooManyLoginsError();
	let result;
	try {
		result = await login(c.app, input.username, input.password);
	} catch (err) {
		if (err instanceof BadCredentialsError) c.app.logins.fail(key);
		throw err;
	}
	c.app.logins.reset(key);
	setSessionCookie(c, result.token, SESSION_MAX_AGE_SECONDS);
	writeJSON(c.res, 200, publicUser(result.user));
}

async function handleLogout(c: Ctx): Promise<void> {
	const token = sessionToken(c.req);
	if (token !== "") await logout(c.app, token);
	setSessionCookie(c, "", -1);
	c.res.writeHead(204).end();
}

async function handleMe(c: Ctx): Promise<void> {
	writeJSON(c.res, 200, publicUser(c.user!));
}

async function handleChangePassword(c: Ctx): Promise<void> {
	const input = await decodeBody(c, true, decodePasswordChange);
	await changePassword(c.app, c.user!, c.token, input);
	c.res.writeHead(204).end();
}

async function handleListCurrencies(c: Ctx): Promise<void> {
	writeJSON(c.res, 200, { data: await listCurrencies(c.app) });
}

async function handleListAccounts(c: Ctx): Promise<void> {
	const includeInactive = queryBool(c.query, "include_inactive");
	writeJSON(c.res, 200, { data: await listAccounts(c.app, includeInactive) });
}

async function handleCreateAccount(c: Ctx): Promise<void> {
	const input = await decodeBody(c, true, decodeAccountCreate);
	writeJSON(c.res, 201, await createAccount(c.app, input));
}

async function handleGetAccount(c: Ctx): Promise<void> {
	writeJSON(c.res, 200, await getAccount(c.app, c.params.accountCode!));
}

async function handleUpdateAccount(c: Ctx): Promise<void> {
	const input = await decodeBody(c, true, decodeAccountUpdate);
	writeJSON(c.res, 200, await updateAccount(c.app, c.params.accountCode!, input));
}

async function handleAccountLedger(c: Ctx): Promise<void> {
	const [from, to] = queryDateRange(c.query);
	writeJSON(c.res, 200, await getAccountLedger(c.app, c.params.accountCode!, from, to));
}

async function handleListJournalEntries(c: Ctx): Promise<void> {
	const [from, to] = queryDateRange(c.query);
	const f: EntryFilter = { from, to, accountCode: null, limit: 50, offset: 0 };
	const v = new ValidationError();
	const code = c.query.get("account_code") ?? "";
	if (code !== "") {
		if (!ACCOUNT_CODE_RE.test(code)) v.add("account_code", "is not a valid account code");
		else f.accountCode = code;
	}
	const limit = c.query.get("limit") ?? "";
	if (limit !== "") {
		const n = atoi(limit);
		if (n === null || n < 1 || n > 500) v.add("limit", "must be an integer between 1 and 500");
		else f.limit = n;
	}
	const offset = c.query.get("offset") ?? "";
	if (offset !== "") {
		const n = atoi(offset);
		if (n === null || n < 0) v.add("offset", "must be a non-negative integer");
		else f.offset = n;
	}
	v.throwIfAny();
	writeJSON(c.res, 200, { data: await listJournalEntries(c.app, f), limit: f.limit, offset: f.offset });
}

async function handleCreateJournalEntry(c: Ctx): Promise<void> {
	const input = await decodeBody(c, true, decodeJournalEntryCreate);
	writeJSON(c.res, 201, await createJournalEntry(c.app, input, c.user!.id));
}

async function handleGetJournalEntry(c: Ctx): Promise<void> {
	writeJSON(c.res, 200, await getJournalEntry(c.app, pathId(c, "entryId")));
}

async function handleReverseJournalEntry(c: Ctx): Promise<void> {
	const id = pathId(c, "entryId");
	const input = await decodeBody(c, false, decodeReverseRequest);
	writeJSON(c.res, 201, await reverseJournalEntry(c.app, id, input, c.user!.id));
}

async function handleTrialBalance(c: Ctx): Promise<void> {
	const asOf = queryDate(c.query, "as_of");
	const currency = queryCurrency(c.query);
	writeJSON(c.res, 200, await getTrialBalance(c.app, asOf, currency));
}

async function handleBalanceSheet(c: Ctx): Promise<void> {
	const asOf = queryDate(c.query, "as_of");
	const currency = queryCurrency(c.query);
	writeJSON(c.res, 200, await getBalanceSheet(c.app, asOf, currency));
}

async function handleIncomeStatement(c: Ctx): Promise<void> {
	const [from, to] = queryDateRange(c.query);
	const currency = queryCurrency(c.query);
	writeJSON(c.res, 200, await getIncomeStatement(c.app, from, to, currency));
}

async function handleExportGeneralLedger(c: Ctx): Promise<void> {
	const input = await decodeBody(c, false, decodeExportRequest);
	writeJSON(c.res, 201, await exportGeneralLedger(c.app, input.from, input.to, input.currency));
}

async function handleListFiles(c: Ctx): Promise<void> {
	writeJSON(c.res, 200, { data: await listCsvFiles(c.app) });
}

async function handleDownloadFile(c: Ctx): Promise<void> {
	const name = c.params.filename!;
	const { fh, info } = await openCsvFile(c.app, name);
	try {
		const modified = new Date(Math.floor(info.mtimeMs / 1000) * 1000);
		const since = Date.parse(c.req.headers["if-modified-since"] ?? "");
		if (!Number.isNaN(since) && modified.getTime() <= since) {
			c.res.writeHead(304, { "Last-Modified": modified.toUTCString() }).end();
			return;
		}
		c.res.writeHead(200, {
			"Content-Type": "text/csv; charset=utf-8",
			"Content-Disposition": `attachment; filename="${name}"`,
			"Content-Length": info.size,
			"Last-Modified": modified.toUTCString(),
		});
		await new Promise<void>((resolve, reject) => {
			const stream = fh.createReadStream({ autoClose: false, start: 0, end: Math.max(info.size - 1, 0) });
			if (info.size === 0) {
				stream.destroy();
				c.res.end();
				resolve();
				return;
			}
			stream.on("error", reject);
			c.res.on("close", resolve);
			stream.pipe(c.res);
		});
	} finally {
		await fh.close();
	}
}

async function handleUploadFile(c: Ctx): Promise<void> {
	const boundary = formDataBoundary(c.req.headers["content-type"]);
	if (!boundary) throw new HttpError(400, "bad_request", 'expected a multipart/form-data body with a "file" field');
	let body: Buffer;
	try {
		body = await readBody(c.req, MAX_FILE_BYTES + MULTIPART_OVERHEAD);
	} catch (err) {
		if (err instanceof BodyTooLargeError) {
			c.res.setHeader("Connection", "close");
			throw new TooLargeError();
		}
		throw err;
	}
	let parts;
	try {
		parts = parseMultipart(body, boundary);
	} catch (err) {
		if (err instanceof MultipartError) throw new HttpError(400, "bad_request", "invalid multipart body: " + err.message);
		throw err;
	}
	const part = parts.find((p) => p.name === "file");
	if (!part) throw new HttpError(400, "bad_request", 'missing "file" field');
	writeJSON(c.res, 201, await saveUploadedCsv(c.app, part.filename, part.data));
}

async function handleImportJournalEntries(c: Ctx): Promise<void> {
	const input = await decodeBody(c, true, decodeImportRequest);
	if (!CSV_FILENAME_RE.test(input.filename)) {
		const v = new ValidationError();
		v.add("filename", "must be a .csv file name in the CSV directory");
		throw v;
	}
	writeJSON(c.res, 201, await importJournalEntries(c.app, input.filename, c.user!.id));
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

function route(method: string, pattern: string, auth: boolean, handler: Handler): Route {
	return { method, segments: pattern.split("/").slice(1), auth, handler };
}

const ROUTES: Route[] = [
	route("GET", "/health", false, handleHealth),
	route("POST", "/auth/login", false, handleLogin),
	route("POST", "/auth/logout", false, handleLogout),

	// Everything else requires a logged-in user.
	route("GET", "/auth/me", true, handleMe),
	route("POST", "/auth/password", true, handleChangePassword),
	route("GET", "/currencies", true, handleListCurrencies),

	route("GET", "/accounts", true, handleListAccounts),
	route("POST", "/accounts", true, handleCreateAccount),
	route("GET", "/accounts/:accountCode", true, handleGetAccount),
	route("PATCH", "/accounts/:accountCode", true, handleUpdateAccount),
	route("GET", "/accounts/:accountCode/ledger", true, handleAccountLedger),

	route("GET", "/journal-entries", true, handleListJournalEntries),
	route("POST", "/journal-entries", true, handleCreateJournalEntry),
	route("GET", "/journal-entries/:entryId", true, handleGetJournalEntry),
	route("POST", "/journal-entries/:entryId/reverse", true, handleReverseJournalEntry),

	route("GET", "/reports/trial-balance", true, handleTrialBalance),
	route("GET", "/reports/balance-sheet", true, handleBalanceSheet),
	route("GET", "/reports/income-statement", true, handleIncomeStatement),

	route("POST", "/exports/general-ledger", true, handleExportGeneralLedger),
	route("GET", "/files", true, handleListFiles),
	route("POST", "/files", true, handleUploadFile),
	route("GET", "/files/:filename", true, handleDownloadFile),
	route("POST", "/imports/journal-entries", true, handleImportJournalEntries),
];

/** Matches a path relative to /api/v1 against a route; returns its decoded params. */
function match(r: Route, segments: string[]): Record<string, string> | null {
	if (r.segments.length !== segments.length) return null;
	const params: Record<string, string> = {};
	for (const [i, seg] of r.segments.entries()) {
		const actual = segments[i]!;
		if (seg.startsWith(":")) {
			if (actual === "") return null;
			params[seg.slice(1)] = actual; // kept percent-encoded, so "%2F" can never become a path separator
		} else if (seg !== actual) {
			return null;
		}
	}
	return params;
}

async function serveAPI(c: Ctx, rel: string): Promise<void> {
	const segments = rel.split("/").slice(1);
	let pathMatched = false;
	for (const r of ROUTES) {
		const params = match(r, segments);
		if (!params) continue;
		pathMatched = true;
		if (r.method !== c.req.method) continue;
		c.params = params;
		if (r.auth) {
			c.token = sessionToken(c.req);
			c.user = await sessionUser(c.app, c.token);
		}
		await r.handler(c);
		return;
	}
	if (pathMatched) writeError(c.res, 405, "bad_request", "method not allowed");
	else writeError(c.res, 404, "not_found", "no such endpoint");
}

/**
 * Rejects cross-site state-changing requests (checks Sec-Fetch-Site, then
 * Origin against Host), so another web page open in the same browser cannot
 * post entries or upload files to this server.
 */
function crossOriginAllowed(req: IncomingMessage): boolean {
	if (req.method === "GET" || req.method === "HEAD" || req.method === "OPTIONS") return true;
	const site = req.headers["sec-fetch-site"] ?? "";
	if (site === "same-origin" || site === "none") return true;
	if (site !== "") return false;
	const origin = req.headers.origin ?? "";
	if (origin === "") return true; // not a browser
	const m = /^[A-Za-z][A-Za-z0-9+.-]*:\/\/(?:[^@/?#]*@)?([^/?#]*)/.exec(origin);
	return m !== null && m[1] === req.headers.host;
}

let requestCounter = 0;
const requestPrefix = randomBytes(4).toString("hex");

/** Builds the request handler for the HTTP server. */
export function createHandler(app: App): (req: IncomingMessage, res: ServerResponse) => void {
	const serveStatic = staticHandler(app.webDir);
	return (req, res) => {
		const start = performance.now();
		const requestId = `${requestPrefix}-${String(++requestCounter).padStart(6, "0")}`;
		const rawUrl = req.url ?? "/";
		const q = rawUrl.indexOf("?");
		const pathname = q < 0 ? rawUrl : rawUrl.slice(0, q);
		res.on("finish", () => {
			log.info("request", {
				method: req.method,
				path: pathname,
				status: res.statusCode,
				duration_ms: Math.round(performance.now() - start),
				request_id: requestId,
			});
		});

		res.setHeader("X-Content-Type-Options", "nosniff");
		res.setHeader("X-Frame-Options", "DENY");
		res.setHeader("Referrer-Policy", "same-origin");

		const c: Ctx = { app, req, res, params: {}, query: new URLSearchParams(q < 0 ? "" : rawUrl.slice(q + 1)), user: null, token: "" };
		const run = async () => {
			if (!crossOriginAllowed(req)) {
				writeError(res, 403, "forbidden", "cross-origin request rejected");
			} else if (pathname === "/api/v1" || pathname.startsWith("/api/v1/")) {
				await serveAPI(c, pathname.slice("/api/v1".length));
			} else if (pathname.startsWith("/api/")) {
				writeError(res, 404, "not_found", "no such endpoint");
			} else {
				await serveStatic(req, res, pathname);
			}
		};
		run().catch((err) => {
			try {
				writeErr(c, err);
			} catch (e) {
				log.error("write error response", { err: String(e) });
				res.destroy();
			}
		});
	};
}
