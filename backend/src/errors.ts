// Errors shared by ledger operations. http.ts maps them to status codes.

export interface FieldError {
	field?: string;
	message: string;
}

// Caps the details returned for one request (e.g. a large CSV import full of
// mistakes).
export const MAX_REPORTED_ERRORS = 100;

/** Reports every problem found in a request at once (HTTP 422). */
export class ValidationError extends Error {
	details: FieldError[] = [];

	constructor() {
		super("validation failed");
	}

	add(field: string, message: string): void {
		if (this.details.length === MAX_REPORTED_ERRORS) {
			this.details.push({ message: "too many errors; further errors omitted" });
		}
		if (this.details.length > MAX_REPORTED_ERRORS) return;
		this.details.push(field ? { field, message } : { message });
		this.message = "validation failed: " + this.details.map((d) => (d.field ? `${d.field}: ${d.message}` : d.message)).join("; ");
	}

	get size(): number {
		return this.details.length;
	}

	/** Throws this error if any problem was recorded. */
	throwIfAny(): void {
		if (this.details.length > 0) throw this;
	}
}

/** Throws a ValidationError with a single problem. */
export function invalid(field: string, message: string): never {
	const v = new ValidationError();
	v.add(field, message);
	throw v;
}

/** A well-formed request that clashes with existing ledger state (HTTP 409). */
export class ConflictError extends Error {}

export class NotFoundError extends Error {
	constructor() {
		super("not found");
	}
}

export class UnauthorizedError extends Error {
	constructor() {
		super("unauthorized");
	}
}

/** Deliberately vague: does not reveal whether the username exists or is disabled. */
export class BadCredentialsError extends Error {
	constructor() {
		super("invalid username or password");
	}
}

export class TooManyLoginsError extends Error {
	constructor() {
		super("too many failed login attempts; try again later");
	}
}

/** An upload larger than the limit (HTTP 413). */
export class TooLargeError extends Error {
	constructor() {
		super("file too large");
	}
}
