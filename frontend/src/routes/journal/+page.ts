import { api, unwrapLoad } from '#lib/api/client.ts';
import type { PageLoad } from './$types';

export const _PAGE_SIZE = 50;

export const load: PageLoad = async ({ fetch, url }) => {
	const q = url.searchParams;
	const filter = {
		from: q.get('from') ?? '',
		to: q.get('to') ?? '',
		account_code: q.get('account_code') ?? '',
		offset: Math.max(0, Number.parseInt(q.get('offset') ?? '0', 10) || 0)
	};
	const client = api(fetch);
	const [entries, accounts] = await Promise.all([
		client
			.GET('/journal-entries', {
				params: {
					query: {
						from: filter.from || undefined,
						to: filter.to || undefined,
						account_code: filter.account_code || undefined,
						limit: _PAGE_SIZE,
						offset: filter.offset
					}
				}
			})
			.then(unwrapLoad),
		client.GET('/accounts', { params: { query: { include_inactive: true } } }).then(unwrapLoad)
	]);
	return { entries: entries.data, accounts: accounts.data, filter };
};
