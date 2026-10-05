import { api, today, unwrapLoad, type Currency } from '#lib/api/client.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch, url }) => {
	const asOf = url.searchParams.get('as_of') || today();
	const c = url.searchParams.get('currency');
	const currency = c === 'CAD' || c === 'USD' ? (c as Currency) : undefined;
	const res = await api(fetch).GET('/reports/balance-sheet', { params: { query: { as_of: asOf, currency } } });
	return { bs: unwrapLoad(res), asOf, currency: currency ?? '' };
};
