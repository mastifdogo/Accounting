import { trimSpace } from "./text.ts";

// All money is integer cents held as bigint; floating point is never used.
// In the database a line is one signed amount (debit > 0, credit < 0); at
// the API boundary debits and credits are separate non-negative fields.

/** Largest amount accepted at the API boundary (2^53 - 1), exact in JavaScript clients. */
export const MAX_CENTS = 2n ** 53n - 1n;

/** Renders cents as a decimal string ("-1234.05") using integer arithmetic only. */
export function formatCents(c: bigint): string {
	const sign = c < 0n ? "-" : "";
	const u = c < 0n ? -c : c;
	return `${sign}${u / 100n}.${String(u % 100n).padStart(2, "0")}`;
}

const AMOUNT_RE = /^([0-9]+)(?:\.([0-9]{1,2}))?$/;

/**
 * Parses a non-negative decimal amount with at most two fraction digits
 * ("1234", "1234.5", "1234.56") into cents. An empty string is zero.
 * Thousands separators, signs, currency symbols and exponents are rejected.
 */
export function parseCents(input: string): bigint {
	const s = trimSpace(input);
	if (s === "") return 0n;
	const m = AMOUNT_RE.exec(s);
	if (!m) throw new Error(`invalid amount ${JSON.stringify(s)}: use digits with up to two decimals, e.g. 1234.56`);
	const c = BigInt(m[1]!) * 100n + BigInt((m[2] ?? "").padEnd(2, "0"));
	if (c > MAX_CENTS) throw new Error(`amount ${JSON.stringify(s)} is too large`);
	return c;
}

/** Converts a signed database amount into debit/credit columns. */
export function splitAmount(amount: bigint): [debit: bigint, credit: bigint] {
	return amount >= 0n ? [amount, 0n] : [0n, -amount];
}
