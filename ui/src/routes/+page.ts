import { listRuns } from '$lib/api';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ fetch, url }) => {
	const limit = Number(url.searchParams.get('limit')) || undefined;
	const runs = await listRuns(fetch, limit);
	return { runs };
};
