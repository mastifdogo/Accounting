import { api, unwrapLoad, type Currency } from '#lib/api/client.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch, url }) => {
	const from = url.searchParams.get('from') || undefined;
	const to = url.searchParams.get('to') || undefined;
	const c = url.searchParams.get('currency');
	const currency = c === 'CAD' || c === 'USD' ? (c as Currency) : undefined;
	const res = await api(fetch).GET('/reports/income-statement', { params: { query: { from, to, currency } } });
	return { is: unwrapLoad(res), currency: currency ?? '' };
};
