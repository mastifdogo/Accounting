import { api, unwrapLoad } from '#lib/api/client.ts';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch }) => {
	const res = await api(fetch).GET('/files');
	return { files: unwrapLoad(res).data };
};
