// `ledger user ...`: user management with the server's DATABASE_URL.

import { newApp } from "./app.ts";
import { loadConfig } from "./config.ts";
import { connectDB } from "./db.ts";
import { NotFoundError, ValidationError } from "./errors.ts";
import { isPgError } from "./db.ts";
import { createUser, listUsers, normalizeUsername, setPassword, setUserActive } from "./auth.ts";

export const USER_USAGE = `usage:
  ledger user add <name> [--password-stdin]
  ledger user passwd <name> [--password-stdin]
  ledger user disable <name>
  ledger user enable <name>
  ledger user list`;

class UsageError extends Error {}

function parseArgs(args: string[]): { cmd: string; name: string; fromStdin: boolean } {
	if (args.length === 0) throw new UsageError(USER_USAGE);
	const [cmd, ...rest] = args as [string, ...string[]];
	let name = "";
	let fromStdin = false;
	let flagsDone = false;
	for (const a of rest) {
		const flag = /^--?password-stdin(?:=(.*))?$/.exec(a);
		if (!flagsDone && flag) {
			const val = flag[1];
			if (val === undefined || ["1", "t", "T", "TRUE", "true", "True"].includes(val)) fromStdin = true;
			else if (["0", "f", "F", "FALSE", "false", "False"].includes(val)) fromStdin = false;
			else throw new UsageError(`invalid boolean value ${JSON.stringify(val)} for -password-stdin`);
		} else if (!flagsDone && a === "--") {
			flagsDone = true;
		} else if (!flagsDone && a.startsWith("-") && a !== "-") {
			throw new UsageError(`flag provided but not defined: ${a}\n${USER_USAGE}`);
		} else if (name === "") {
			name = a;
		} else {
			throw new UsageError(USER_USAGE);
		}
	}
	if ((cmd === "list") !== (name === "")) throw new UsageError(USER_USAGE);
	return { cmd, name, fromStdin };
}

/** Reads one line from stdin (for --password-stdin or a pipe). */
async function readLine(): Promise<string> {
	const chunks: Buffer[] = [];
	for await (const chunk of process.stdin) {
		chunks.push(chunk as Buffer);
		if ((chunk as Buffer).includes(0x0a)) break;
	}
	const text = Buffer.concat(chunks).toString("utf8");
	if (text === "") throw new Error("read password from stdin: EOF");
	return text.split("\n")[0]!.replace(/\r$/, "");
}

/** Reads a password from the terminal without echoing it. */
function promptHidden(prompt: string): Promise<string> {
	return new Promise((resolve, reject) => {
		const stdin = process.stdin;
		process.stderr.write(prompt);
		stdin.setRawMode(true);
		stdin.resume();
		let value = "";
		const done = (err?: Error) => {
			stdin.setRawMode(false);
			stdin.pause();
			stdin.off("data", onData);
			process.stderr.write("\n");
			if (err) reject(err);
			else resolve(value);
		};
		const onData = (buf: Buffer) => {
			for (const ch of buf.toString("utf8")) {
				if (ch === "\r" || ch === "\n") return done();
				if (ch === "\u0003") return done(new Error("interrupted"));
				if (ch === "\u0004") return done(value === "" ? new Error("EOF") : undefined);
				if (ch === "\u007f" || ch === "\b") value = [...value].slice(0, -1).join("");
				else if (ch === "\u0015") value = "";
				else value += ch;
			}
		};
		stdin.on("data", onData);
	});
}

/** Prompts twice on a terminal (no echo), or reads one line from stdin. */
async function readPassword(fromStdin: boolean): Promise<string> {
	if (fromStdin || !process.stdin.isTTY) return readLine();
	const p1 = await promptHidden("Password: ");
	const p2 = await promptHidden("Repeat password: ");
	if (p1 !== p2) throw new Error("passwords do not match");
	return p1;
}

function cliError(err: unknown): Error {
	if (err instanceof ValidationError) return new Error(err.message.replace(/^validation failed: /, ""));
	if (err instanceof NotFoundError) return new Error("no such user");
	if (isPgError(err) && err.constraint === "users_username_unique") return new Error("a user with that name already exists");
	return err as Error;
}

const pad = (n: number) => String(n).padStart(2, "0");
const localDate = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
const localDateTime = (d: Date) => `${localDate(d)} ${pad(d.getHours())}:${pad(d.getMinutes())}`;

/** Prints rows as aligned columns separated by two spaces. */
function printTable(rows: string[][]): void {
	const widths = rows[0]!.map((_, i) => Math.max(...rows.map((r) => r[i]!.length)));
	for (const r of rows) {
		process.stdout.write(r.map((cell, i) => (i < r.length - 1 ? cell.padEnd(widths[i]! + 2) : cell)).join("") + "\n");
	}
}

/** Implements `ledger user ...`; returns the process exit code. */
export async function runUserCommand(args: string[]): Promise<void> {
	const { cmd, name, fromStdin } = parseArgs(args);
	if (!["add", "passwd", "disable", "enable", "list"].includes(cmd)) throw new UsageError(USER_USAGE);

	const cfg = loadConfig("");
	const db = await connectDB(cfg);
	const app = newApp(db, cfg);
	try {
		switch (cmd) {
			case "add": {
				const pw = await readPassword(fromStdin);
				const u = await createUser(app, name, pw).catch((e) => Promise.reject(cliError(e)));
				console.log(`created user ${u.username}`);
				break;
			}
			case "passwd": {
				const pw = await readPassword(fromStdin);
				await setPassword(app, name, pw).catch((e) => Promise.reject(cliError(e)));
				console.log(`password updated for ${normalizeUsername(name)}; their sessions were ended`);
				break;
			}
			case "disable":
			case "enable":
				await setUserActive(app, name, cmd === "enable").catch((e) => Promise.reject(cliError(e)));
				console.log(`user ${normalizeUsername(name)} ${cmd}d`);
				break;
			case "list": {
				const users = await listUsers(app);
				printTable([
					["USERNAME", "ACTIVE", "CREATED", "LAST LOGIN"],
					...users.map((u) => [u.username, String(u.is_active), localDate(u.created_at), u.last_login_at ? localDateTime(u.last_login_at) : "never"]),
				]);
				break;
			}
		}
	} finally {
		await db.end();
	}
}
