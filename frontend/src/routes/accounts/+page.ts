import { api, unwrapLoad } from '#lib/api/client.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch, url }) => {
	const includeInactive = url.searchParams.get('inactive') === '1';
	const res = await api(fetch).GET('/accounts', { params: { query: { include_inactive: includeInactive } } });
	return { accounts: unwrapLoad(res).data, includeInactive };
};
