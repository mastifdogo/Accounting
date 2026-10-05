import { api, unwrapLoad } from '#lib/api/client.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch }) => {
	const client = api(fetch);
	const [accounts, entries] = await Promise.all([
		client.GET('/accounts', { params: { query: { include_inactive: true } } }).then(unwrapLoad),
		client.GET('/journal-entries', { params: { query: { limit: 10 } } }).then(unwrapLoad)
	]);
	return { accounts: accounts.data, entries: entries.data };
};
