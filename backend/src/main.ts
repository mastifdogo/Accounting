// Ledger: a lightweight double-entry accounting server.
//
//   ledger                      run the HTTP server (configuration: src/config.ts)
//   ledger user add <name>      create a user (prompts for a password)
//   ledger user passwd <name>   set a new password (ends their sessions)
//   ledger user disable <name>  block logins (ends their sessions)
//   ledger user enable <name>
//   ledger user list
//
// Pass --password-stdin to add/passwd to read the password from stdin.

import { createServer } from "node:http";
import { mkdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { newApp } from "./app.ts";
import { loadConfig, parseListenAddr } from "./config.ts";
import { connectDB } from "./db.ts";
import { createHandler } from "./http.ts";
import { log } from "./log.ts";
import { runUserCommand } from "./cli.ts";

/** The built frontend: "web" next to the server bundle, or frontend/build in a checkout. */
function defaultWebDir(): string {
	const here = path.dirname(fileURLToPath(import.meta.url));
	return path.basename(here) === "src" ? path.join(here, "..", "..", "frontend", "build") : path.join(here, "web");
}

async function serve(): Promise<void> {
	const cfg = loadConfig(defaultWebDir());
	const db = await connectDB(cfg);

	try {
		mkdirSync(cfg.csvDir, { recursive: true, mode: 0o750 });
	} catch (err) {
		log.warn("CSV directory is not available; exports will fail", { dir: cfg.csvDir, err: (err as Error).message });
	}

	const app = newApp(db, cfg);
	try {
		const r = await db.query<{ n: bigint }>(`SELECT count(*) AS n FROM users WHERE is_active`);
		if (r.rows[0]!.n === 0n) log.warn("no active users: create one with `ledger user add <name>` to log in");
	} catch {
		// reported by /health and on first use
	}

	const server = createServer(
		{
			headersTimeout: 10_000,
			requestTimeout: 30_000, // whole request, including an upload body
			keepAliveTimeout: 120_000,
		},
		createHandler(app),
	);
	const { host, port } = parseListenAddr(cfg.httpAddr);
	await new Promise<void>((resolve, reject) => {
		server.once("error", reject);
		server.listen({ host, port }, () => {
			server.off("error", reject);
			resolve();
		});
	});
	log.info("listening", { addr: cfg.httpAddr, csv_dir: cfg.csvDir });

	let stopping = false;
	const shutdown = () => {
		if (stopping) return;
		stopping = true;
		log.info("shutting down");
		const force = setTimeout(() => server.closeAllConnections(), 15_000);
		force.unref();
		server.close(() => {
			clearTimeout(force);
			void db.end().finally(() => process.exit(0));
		});
		server.closeIdleConnections();
	};
	process.on("SIGTERM", shutdown);
	process.on("SIGINT", shutdown);
}

async function main(): Promise<void> {
	const major = Number(process.versions.node.split(".")[0]);
	if (major < 22) {
		process.stderr.write(`ledger needs Node.js 22 or newer (this is ${process.version})\n`);
		process.exit(1);
	}
	process.title = "ledger";
	const [, , command, ...args] = process.argv;
	if (command === "user") {
		try {
			await runUserCommand(args);
		} catch (err) {
			process.stderr.write(`error: ${(err as Error).message}\n`);
			process.exitCode = 1;
		}
		return;
	}
	try {
		await serve();
	} catch (err) {
		log.error("fatal", { err: (err as Error).message });
		process.exit(1);
	}
}

void main();
