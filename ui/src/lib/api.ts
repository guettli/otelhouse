import type { Run, Trace, LogRecord } from './types';

// fetch can be either window.fetch (browser/SSR fallback) or the
// load-function-scoped fetch SvelteKit injects so cookies/caching work
// correctly during SSR — accept any of them.
type FetchLike = typeof globalThis.fetch;

async function getJSON<T>(fetcher: FetchLike, path: string): Promise<T> {
	const res = await fetcher(path);
	if (!res.ok) {
		let detail = '';
		try {
			const body = (await res.json()) as { error?: string };
			detail = body.error ?? '';
		} catch {
			// non-JSON body — leave detail empty.
		}
		throw new Error(`GET ${path} failed: ${res.status} ${res.statusText}${detail ? ` — ${detail}` : ''}`);
	}
	return (await res.json()) as T;
}

export function listRuns(fetcher: FetchLike, limit?: number): Promise<Run[]> {
	const qs = limit ? `?limit=${limit}` : '';
	return getJSON<Run[]>(fetcher, `/api/runs${qs}`);
}

export function getTrace(fetcher: FetchLike, traceID: string): Promise<Trace> {
	return getJSON<Trace>(fetcher, `/api/traces/${encodeURIComponent(traceID)}`);
}

export function getLogs(fetcher: FetchLike, traceID: string): Promise<LogRecord[]> {
	return getJSON<LogRecord[]>(fetcher, `/api/logs?traceId=${encodeURIComponent(traceID)}`);
}
