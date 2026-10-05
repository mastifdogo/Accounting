import { error } from '@sveltejs/kit';
import { api, unwrapLoad } from '#lib/api/client.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch, params }) => {
	const id = Number(params.id);
	if (!Number.isSafeInteger(id) || id < 1) error(404, 'Not found');
	const res = await api(fetch).GET('/journal-entries/{entryId}', { params: { path: { entryId: id } } });
	return { entry: unwrapLoad(res) };
};
