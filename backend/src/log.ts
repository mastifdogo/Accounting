// Structured JSON log lines on stdout (collected by journald).

function write(level: string, msg: string, attrs: Record<string, unknown>): void {
	process.stdout.write(JSON.stringify({ time: new Date().toISOString(), level, msg, ...attrs }) + "\n");
}

export const log = {
	info: (msg: string, attrs: Record<string, unknown> = {}) => write("INFO", msg, attrs),
	warn: (msg: string, attrs: Record<string, unknown> = {}) => write("WARN", msg, attrs),
	error: (msg: string, attrs: Record<string, unknown> = {}) => write("ERROR", msg, attrs),
};
