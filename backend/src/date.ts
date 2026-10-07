// Calendar days are plain "YYYY-MM-DD" strings (OpenAPI format: date). The
// fixed-width format sorts and compares correctly as text.

const DATE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

function daysInMonth(year: number, month: number): number {
	if (month === 2) return (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0 ? 29 : 28;
	return [4, 6, 9, 11].includes(month) ? 30 : 31;
}

/** Validates a "YYYY-MM-DD" calendar date and returns it. */
export function parseDate(s: string): string {
	const m = DATE_RE.exec(s);
	if (m) {
		const year = Number(m[1]);
		const month = Number(m[2]);
		const day = Number(m[3]);
		if (month >= 1 && month <= 12 && day >= 1 && day <= daysInMonth(year, month)) return s;
	}
	throw new Error(`invalid date ${JSON.stringify(s)}, expected YYYY-MM-DD`);
}

/** Today's date in UTC. */
export function today(): string {
	return new Date().toISOString().slice(0, 10);
}

export function dateYear(d: string): number {
	return Number(d.slice(0, 4));
}
