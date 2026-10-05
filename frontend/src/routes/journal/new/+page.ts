import { api, unwrapLoad } from '#lib/api/client.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch }) => {
	const res = await api(fetch).GET('/accounts', { params: { query: { include_inactive: false } } });
	return { accounts: unwrapLoad(res).data };
};
