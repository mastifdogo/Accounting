import { redirect } from '@sveltejs/kit';
import { api } from '#lib/api/client.ts';
import type { LayoutLoad } from './$types';

// Single-page app served by the Go backend: render in the browser only.
export const ssr = false;
export const prerender = false;

// Resolve the logged-in user on every navigation; send anonymous visitors to
// the login page (the API enforces authentication regardless).
export const load: LayoutLoad = async ({ fetch, url, depends }) => {
	depends('app:user');
	const res = await api(fetch).GET('/auth/me');
	const user = res.data ?? null;
	const onLogin = url.pathname === '/login';
	if (!user && !onLogin) {
		if (res.response.status !== 401) {
			// API down or misconfigured: let the page show the error.
			return { user: null };
		}
		redirect(307, `/login?next=${encodeURIComponent(url.pathname + url.search)}`);
	}
	if (user && onLogin) redirect(307, safeNext(url.searchParams.get('next')));
	return { user };
};

/** Only same-site relative paths are followed after login. */
export function _safeNext(next: string | null): string {
	return next && next.startsWith('/') && !next.startsWith('//') && !next.startsWith('/\\') ? next : '/';
}
const safeNext = _safeNext;
