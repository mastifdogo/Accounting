// Money helpers. Amounts are integer cents everywhere; these functions use
// BigInt for parsing and formatting so no value ever passes through
// floating-point arithmetic.

/** Largest amount the API accepts (2^53 - 1 cents). */
export const MAX_CENTS = Number.MAX_SAFE_INTEGER;

const AMOUNT_RE = /^(\d{1,3}(?:,\d{3})+|\d+)(?:\.(\d{1,2}))?$/;

/**
 * Parses a user-entered, non-negative amount such as "1234.5" or "1,234.56"
 * into cents. Returns 0 for an empty string and null for anything invalid
 * (signs, more than two decimals, letters, malformed thousands separators).
 */
export function parseAmount(input: string): number | null {
	const s = input.trim();
	if (s === '') return 0;
	const m = AMOUNT_RE.exec(s);
	if (!m) return null;
	const whole = BigInt(m[1].replaceAll(',', ''));
	const frac = BigInt((m[2] ?? '').padEnd(2, '0'));
	const cents = whole * 100n + frac;
	if (cents > BigInt(MAX_CENTS)) return null;
	return Number(cents);
}

/** Formats cents as "1,234.56" (or "-1,234.56"). */
export function formatCents(cents: number): string {
	const b = BigInt(cents);
	const neg = b < 0n;
	const abs = neg ? -b : b;
	const whole = (abs / 100n).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',');
	const frac = (abs % 100n).toString().padStart(2, '0');
	return `${neg ? '-' : ''}${whole}.${frac}`;
}

/** Formats cents for an input field: "1234.56", or "" for zero. */
export function centsToInput(cents: number): string {
	return cents === 0 ? '' : formatCents(cents).replaceAll(',', '');
}

/** Formats cents with a currency code: "1,234.56 CAD". */
export function money(cents: number, currency: string): string {
	return `${formatCents(cents)} ${currency}`;
}

/** Normal balance side of each account type. */
export function isDebitNormal(type: string): boolean {
	return type === 'asset' || type === 'expense';
}

/**
 * Converts a debit-positive balance into the account's natural sign: assets
 * and expenses stay as is; liabilities, equity and revenue are flipped so a
 * normal balance shows as positive.
 */
export function naturalBalance(type: string, debitPositiveCents: number): number {
	return isDebitNormal(type) ? debitPositiveCents : -debitPositiveCents;
}
