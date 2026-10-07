import type { ValidationError } from "./errors.ts";

// Unicode White_Space, as trimmed by Go's strings.TrimSpace (unlike
// String.prototype.trim, U+FEFF is not whitespace and U+0085 is).
const SPACE = "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const TRIM_RE = new RegExp(`^[${SPACE}]+|[${SPACE}]+$`, "g");
const LEADING_SPACE_RE = new RegExp(`^[${SPACE}]`);

export function trimSpace(s: string): string {
	return s.replace(TRIM_RE, "");
}

export function startsWithSpace(s: string): boolean {
	return LEADING_SPACE_RE.test(s);
}

/** Length in Unicode code points, matching PostgreSQL's length(). */
export function charCount(s: string): number {
	let n = 0;
	for (const _ of s) n++;
	return n;
}

/** Quotes a value for an error message, like Go's %q. */
export function quote(s: string): string {
	return JSON.stringify(s);
}

const LONE_SURROGATE_RE = /\p{Cs}/u;

/**
 * Validates a free-text field's length in characters and rejects text
 * PostgreSQL cannot store (NUL, or unpaired surrogates that are not valid
 * UTF-8).
 */
export function checkText(v: ValidationError, field: string, s: string, minLen: number, maxLen: number): void {
	if (LONE_SURROGATE_RE.test(s)) {
		v.add(field, "must be valid UTF-8");
		return;
	}
	if (s.includes("\0")) {
		v.add(field, "must not contain NUL characters");
		return;
	}
	if (minLen > 0 && charCount(trimSpace(s)) < minLen) {
		v.add(field, "is required");
	} else if (charCount(s) > maxLen) {
		v.add(field, `must be at most ${maxLen} characters`);
	}
}
