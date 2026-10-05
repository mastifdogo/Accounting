// Typed API client. Every request and response type is generated from
// api/openapi.yaml (`npm run gen:api`), so the frontend only depends on the
// published contract, never on backend internals.

import createClient from 'openapi-fetch';
import { error as kitError } from '@sveltejs/kit';
import type { components, paths } from './schema';

export type Schemas = components['schemas'];
export type Account = Schemas['Account'];
export type AccountType = Schemas['AccountType'];
export type Currency = Schemas['Currency'];
export type JournalEntry = Schemas['JournalEntry'];
export type JournalLineCreate = Schemas['JournalLineCreate'];
export type TrialBalance = Schemas['TrialBalance'];
export type CsvFile = Schemas['CsvFile'];
export type ImportResult = Schemas['ImportResult'];
export type ApiErrorBody = Schemas['Error']['error'];

export const API_BASE = '/api/v1';
export const ACCOUNT_TYPES: AccountType[] = ['asset', 'liability', 'equity', 'revenue', 'expense'];
export const CURRENCIES: Currency[] = ['CAD', 'USD'];

/** Creates a client. Pass SvelteKit's `fetch` inside load functions. */
export function api(fetchFn: typeof fetch = fetch) {
	return createClient<paths>({ baseUrl: API_BASE, fetch: fetchFn });
}

/** An API error response, carrying the server's message and field details. */
export class ApiError extends Error {
	constructor(
		public status: number,
		public body: ApiErrorBody
	) {
		super(body.message);
	}

	/** Field-level messages, e.g. `lines[1].account_code: account "9" does not exist`. */
	get details(): string[] {
		return (this.body.details ?? []).map((d) => (d.field ? `${d.field}: ${d.message}` : d.message));
	}
}

type Result<T> = { data?: T; error?: unknown; response: Response };

function toApiError(res: Result<unknown>): ApiError {
	const body = (res.error as { error?: ApiErrorBody } | undefined)?.error;
	return new ApiError(res.response.status, body ?? { code: 'internal_error', message: res.response.statusText || 'Request failed' });
}

/** Returns the response data or throws an ApiError (for use in event handlers). */
export function unwrap<T>(res: Result<T>): T {
	if (res.error !== undefined || res.data === undefined) throw toApiError(res);
	return res.data;
}

/** Returns the response data or raises a SvelteKit error page (for use in load functions). */
export function unwrapLoad<T>(res: Result<T>): T {
	if (res.error !== undefined || res.data === undefined) {
		const e = toApiError(res);
		kitError(e.status, e.message);
	}
	return res.data as T;
}

/** Uploads a CSV file (multipart/form-data, field "file"). */
export async function uploadCsv(file: File): Promise<CsvFile> {
	const form = new FormData();
	form.append('file', file);
	const response = await fetch(`${API_BASE}/files`, { method: 'POST', body: form });
	const body = await response.json().catch(() => undefined);
	if (!response.ok) {
		throw new ApiError(response.status, body?.error ?? { code: 'internal_error', message: response.statusText });
	}
	return body as CsvFile;
}

/** Converts any thrown value into display text plus optional details. */
export function describeError(e: unknown): { message: string; details: string[] } {
	if (e instanceof ApiError) return { message: e.message, details: e.details };
	if (e instanceof Error) return { message: e.message, details: [] };
	return { message: String(e), details: [] };
}

/** Today's date (local time) as YYYY-MM-DD. */
export function today(): string {
	const d = new Date();
	const pad = (n: number) => String(n).padStart(2, '0');
	return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}
