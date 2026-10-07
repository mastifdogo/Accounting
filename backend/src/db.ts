import pg from "pg";
import type { Config } from "./config.ts";
import { log } from "./log.ts";

// Type parsing: BIGINT as bigint (money and ids are never squeezed through a
// float), DATE as the plain "YYYY-MM-DD" text, everything else as pg does.
const INT8 = 20;
const DATE = 1082;

function getTypeParser(oid: number, format?: string): (value: string) => unknown {
	if (format !== "binary") {
		if (oid === INT8) return (v: string) => BigInt(v);
		if (oid === DATE) return (v: string) => v;
	}
	return pg.types.getTypeParser(oid, format as "text");
}

export type Pool = pg.Pool;
export type Queryable = pg.Pool | pg.PoolClient;

/** Every request's SQL must finish within this time (HTTP handlers' budget). */
export const STATEMENT_TIMEOUT_MS = 60_000;

/**
 * Opens a small connection pool sized for a low-resource container and
 * verifies connectivity.
 */
export async function connectDB(cfg: Pick<Config, "databaseUrl" | "dbMaxConns">): Promise<pg.Pool> {
	const pool = new pg.Pool({
		connectionString: cfg.databaseUrl,
		max: cfg.dbMaxConns,
		min: 0,
		idleTimeoutMillis: 5 * 60_000,
		maxLifetimeSeconds: 3600,
		connectionTimeoutMillis: 10_000,
		// Store and compare timestamps in UTC regardless of server defaults.
		options: "-c TimeZone=UTC",
		statement_timeout: STATEMENT_TIMEOUT_MS,
		types: { getTypeParser } as pg.CustomTypesConfig,
	});
	// An idle client losing its connection must not crash the process.
	pool.on("error", (err) => log.warn("idle database connection error", { err: err.message }));
	try {
		await pool.query("SELECT 1");
	} catch (err) {
		await pool.end().catch(() => {});
		throw new Error(`ping database: ${(err as Error).message}`, { cause: err });
	}
	return pool;
}

export type IsolationLevel = "READ COMMITTED" | "REPEATABLE READ";

/**
 * Runs fn inside BEGIN ... COMMIT on one connection. Any error, including
 * one raised by deferred constraint triggers at COMMIT, rolls everything back.
 */
export async function withTx<T>(
	pool: pg.Pool,
	opts: { isolation?: IsolationLevel; readOnly?: boolean },
	fn: (tx: pg.PoolClient) => Promise<T>,
): Promise<T> {
	const client = await pool.connect();
	let broken: Error | undefined;
	try {
		await client.query(`BEGIN ISOLATION LEVEL ${opts.isolation ?? "READ COMMITTED"}${opts.readOnly ? " READ ONLY" : ""}`);
		try {
			const result = await fn(client);
			await client.query("COMMIT");
			return result;
		} catch (err) {
			await client.query("ROLLBACK").catch((e: Error) => {
				broken = e; // discard this connection instead of reusing it
			});
			throw err;
		}
	} finally {
		client.release(broken);
	}
}

/** Returns the first row, or undefined. */
export async function queryRow<T>(db: Queryable, sql: string, params: unknown[] = []): Promise<T | undefined> {
	const r = await db.query(sql, params);
	return r.rows[0] as T | undefined;
}

export async function queryRows<T>(db: Queryable, sql: string, params: unknown[] = []): Promise<T[]> {
	const r = await db.query(sql, params);
	return r.rows as T[];
}

/** A PostgreSQL error reported by the server (constraint violations, triggers). */
export function isPgError(err: unknown): err is pg.DatabaseError {
	return err instanceof pg.DatabaseError;
}

export function toNumber(v: bigint | number | null): number | null {
	return v === null ? null : Number(v);
}
