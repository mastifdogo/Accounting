// Configuration from environment variables:
//
//   DATABASE_URL   PostgreSQL connection string (required)
//   HTTP_ADDR      listen address               (default ":8080")
//   CSV_DIR        CSV import/export directory  (default "/app/csv_data")
//   DB_MAX_CONNS   connection pool size         (default 4)
//   COOKIE_SECURE  session cookie Secure flag: auto | true | false (default auto)
//   WEB_DIR        built frontend               (default: "web" next to the server bundle)

export type CookieSecure = "auto" | "true" | "false";

export interface Config {
	databaseUrl: string;
	httpAddr: string;
	csvDir: string;
	dbMaxConns: number;
	cookieSecure: CookieSecure;
	webDir: string;
}

function envOr(key: string, def: string): string {
	const v = process.env[key];
	return v ? v : def;
}

export function loadConfig(defaultWebDir: string): Config {
	const cfg: Config = {
		databaseUrl: process.env.DATABASE_URL ?? "",
		httpAddr: envOr("HTTP_ADDR", ":8080"),
		csvDir: envOr("CSV_DIR", "/app/csv_data"),
		dbMaxConns: 4,
		cookieSecure: "auto",
		webDir: envOr("WEB_DIR", defaultWebDir),
	};
	if (!cfg.databaseUrl) throw new Error("DATABASE_URL is required");

	const cs = envOr("COOKIE_SECURE", "auto");
	if (cs !== "auto" && cs !== "true" && cs !== "false") {
		throw new Error(`COOKIE_SECURE must be auto, true or false, got ${JSON.stringify(cs)}`);
	}
	cfg.cookieSecure = cs;

	const conns = process.env.DB_MAX_CONNS;
	if (conns) {
		const n = /^[0-9]+$/.test(conns) ? Number(conns) : NaN;
		if (!(n >= 1 && n <= 2 ** 31 - 1)) {
			throw new Error(`DB_MAX_CONNS must be a positive integer, got ${JSON.stringify(conns)}`);
		}
		cfg.dbMaxConns = n;
	}
	parseListenAddr(cfg.httpAddr); // validate early
	return cfg;
}

/** Splits "host:port", ":port" or "[v6]:port". An empty host means all interfaces. */
export function parseListenAddr(addr: string): { host: string | undefined; port: number } {
	const m = /^(?:\[([^\]]*)\]|([^:]*)):([0-9]{1,5})$/.exec(addr);
	const port = m ? Number(m[3]) : NaN;
	if (!m || port > 65535) throw new Error(`HTTP_ADDR must look like ":8080" or "127.0.0.1:8080", got ${JSON.stringify(addr)}`);
	const host = m[1] ?? m[2];
	return { host: host ? host : undefined, port };
}
