import { getLogs, getTrace } from '$lib/api';
import type { PageLoad } from './$types';

// Trace ids aren't enumerable at build time, so we leave this route to the
// SPA fallback (index.html) emitted by @sveltejs/adapter-static.
export const prerender = false;

export const load: PageLoad = async ({ fetch, params }) => {
	const [trace, logs] = await Promise.all([
		getTrace(fetch, params.id),
		getLogs(fetch, params.id)
	]);
	return { trace, logs };
};
