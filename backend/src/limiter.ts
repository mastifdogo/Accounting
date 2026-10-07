// Login rate limiting, in memory, per client IP + username.

export const LOGIN_MAX_FAILURES = 10;
export const LOGIN_WINDOW_MS = 15 * 60_000;

export class LoginLimiter {
	private failures = new Map<string, number[]>();

	private recent(key: string, now: number): number[] {
		const kept = (this.failures.get(key) ?? []).filter((t) => now - t < LOGIN_WINDOW_MS);
		if (kept.length === 0) this.failures.delete(key);
		else this.failures.set(key, kept);
		return kept;
	}

	allowed(key: string): boolean {
		return this.recent(key, Date.now()).length < LOGIN_MAX_FAILURES;
	}

	fail(key: string): void {
		const now = Date.now();
		this.failures.set(key, [...this.recent(key, now), now]);
		// Bound memory if someone sprays many usernames.
		if (this.failures.size > 10_000) {
			for (const k of [...this.failures.keys()]) this.recent(k, now);
		}
	}

	reset(key: string): void {
		this.failures.delete(key);
	}
}
