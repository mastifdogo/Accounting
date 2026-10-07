// Integration test harness: a throwaway PostgreSQL database with
// ../db/schema.sql applied, and the real HTTP handler on an ephemeral port.
//
// Set TEST_DATABASE_URL to a role that may CREATE DATABASE, e.g.
//   TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres npm run test:integration
// Without it these tests are skipped.

import { request as httpRequest, createServer, type IncomingHttpHeaders, type Server } from "node:http";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import type { AddressInfo } from "node:net";
import type { TestContext } from "node:test";
import pg from "pg";
import { newApp, type App } from "../src/app.ts";
import { connectDB } from "../src/db.ts";
import { createHandler } from "../src/http.ts";
import { createUser, login, SESSION_COOKIE } from "../src/auth.ts";

export interface Resp {
	status: number;
	headers: IncomingHttpHeaders;
	body: Buffer;
	text: string;
	json<T = any>(): T;
}

export interface RequestOptions {
	body?: unknown;
	rawBody?: string | Buffer;
	headers?: Record<string, string>;
	cookie?: string;
}

export interface TestApp {
	app: App;
	/** A raw request without the test user's session. */
	request(method: string, urlPath: string, opts?: RequestOptions): Promise<Resp>;
	/** An API request (path relative to /api/v1) carrying the test user's session. */
	call(method: string, apiPath: string, body?: unknown): Promise<Resp>;
	token: string;
	countRows(table: string): Promise<number>;
}

const repo = path.resolve(import.meta.dirname, "..", "..");

export async function setupTestApp(t: TestContext): Promise<TestApp | null> {
	const adminURL = process.env.TEST_DATABASE_URL;
	if (!adminURL) {
		t.skip("TEST_DATABASE_URL not set");
		return null;
	}
	const admin = new pg.Client({ connectionString: adminURL });
	await admin.connect();
	const dbName = `ledger_test_${process.hrtime.bigint()}`;
	await admin.query(`CREATE DATABASE ${dbName}`);
	const url = new URL(adminURL);
	url.pathname = "/" + dbName;
	const db = await connectDB({ databaseUrl: url.toString(), dbMaxConns: 4 });
	const csvDir = await mkdtemp(path.join(tmpdir(), "ledger-csv-"));

	let server: Server | undefined;
	t.after(async () => {
		await new Promise((resolve) => (server ? server.close(resolve) : resolve(undefined)));
		await db.end();
		await admin.query(`DROP DATABASE IF EXISTS ${dbName} WITH (FORCE)`).catch(() => {});
		await admin.end();
		await rm(csvDir, { recursive: true, force: true });
	});

	// No parameters: the simple protocol runs the multi-statement script as-is.
	await db.query(await readFile(path.join(repo, "db", "schema.sql"), "utf8"));

	const app = newApp(db, { csvDir, cookieSecure: "auto", webDir: path.join(repo, "frontend", "build") });
	await createUser(app, "tester", "correct horse battery");
	const { token } = await login(app, "tester", "correct horse battery");

	server = createServer(createHandler(app));
	server.keepAliveTimeout = 1;
	await new Promise<void>((resolve) => server!.listen(0, "127.0.0.1", resolve));
	const port = (server.address() as AddressInfo).port;

	const request = (method: string, urlPath: string, opts: RequestOptions = {}): Promise<Resp> =>
		new Promise((resolve, reject) => {
			const headers: Record<string, string> = { ...opts.headers };
			let payload: Buffer | undefined;
			if (opts.body !== undefined) {
				payload = Buffer.from(JSON.stringify(opts.body));
				headers["Content-Type"] ??= "application/json";
			} else if (opts.rawBody !== undefined) {
				payload = Buffer.from(opts.rawBody);
			}
			if (payload) headers["Content-Length"] = String(payload.length);
			if (opts.cookie) headers.Cookie = opts.cookie;
			const req = httpRequest({ host: "127.0.0.1", port, method, path: urlPath, headers }, (res) => {
				const chunks: Buffer[] = [];
				res.on("data", (c: Buffer) => chunks.push(c));
				res.on("end", () => {
					const body = Buffer.concat(chunks);
					const text = body.toString("utf8");
					resolve({ status: res.statusCode!, headers: res.headers, body, text, json: () => JSON.parse(text) });
				});
			});
			req.on("error", reject);
			req.end(payload);
		});

	return {
		app,
		token,
		request,
		call: (method, apiPath, body) => request(method, "/api/v1" + apiPath, { body, cookie: `${SESSION_COOKIE}=${token}` }),
		countRows: async (table) => Number((await db.query(`SELECT count(*) AS n FROM ${table}`)).rows[0].n),
	};
}

/** Asserts the status, showing the body on failure. */
export function expectStatus(r: Resp, want: number): void {
	if (r.status !== want) throw new Error(`status = ${r.status}, want ${want}; body: ${r.text}`);
}

/** The session cookie set by a response. */
export function sessionFrom(r: Resp): { value: string; attrs: string[]; header: string } {
	const header = (r.headers["set-cookie"] ?? []).find((c) => c.startsWith(SESSION_COOKIE + "="));
	if (!header) throw new Error(`no session cookie in response (status ${r.status}: ${r.text})`);
	const [pair, ...attrs] = header.split("; ");
	return { value: pair!.slice(SESSION_COOKIE.length + 1), attrs, header };
}

/** Builds a multipart/form-data body with one file field. */
export function multipartBody(filename: string, content: Buffer): { body: Buffer; contentType: string } {
	const boundary = "----ledgertest" + Math.random().toString(16).slice(2);
	const body = Buffer.concat([
		Buffer.from(`--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${filename}"\r\nContent-Type: text/csv\r\n\r\n`),
		content,
		Buffer.from(`\r\n--${boundary}--\r\n`),
	]);
	return { body, contentType: `multipart/form-data; boundary=${boundary}` };
}
