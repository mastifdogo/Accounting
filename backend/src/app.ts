import type { Pool } from "./db.ts";
import type { CookieSecure } from "./config.ts";
import { LoginLimiter } from "./limiter.ts";

/** Shared dependencies for handlers and ledger operations. */
export interface App {
	db: Pool;
	csvDir: string;
	cookieSecure: CookieSecure;
	webDir: string;
	logins: LoginLimiter;
}

export function newApp(db: Pool, cfg: { csvDir: string; cookieSecure: CookieSecure; webDir: string }): App {
	return { db, csvDir: cfg.csvDir, cookieSecure: cfg.cookieSecure, webDir: cfg.webDir, logins: new LoginLimiter() };
}
