import { describe, expect, it, vi } from 'vitest';
import { getLogs, getTrace, listRuns } from '../src/lib/api';
import type { LogRecord, Run, Trace } from '../src/lib/types';

function jsonResponse<T>(payload: T, init: ResponseInit = {}): Response {
	return new Response(JSON.stringify(payload), {
		status: 200,
		headers: { 'Content-Type': 'application/json' },
		...init
	});
}

describe('api client', () => {
	it('listRuns hits /api/runs and decodes the payload', async () => {
		const run: Run = {
			trace_id: 'abc',
			service_name: 'dagger',
			start_time: '2024-01-01T00:00:00Z',
			end_time: '2024-01-01T00:00:10Z',
			duration_ns: 10_000_000_000,
			span_count: 3,
			status_code: 'STATUS_CODE_OK',
			command: 'dagger call test',
			resource_attributes: { 'service.name': 'dagger' }
		};
		const fetcher = vi.fn().mockResolvedValue(jsonResponse([run]));
		const runs = await listRuns(fetcher, 25);
		expect(fetcher).toHaveBeenCalledWith('/api/runs?limit=25');
		expect(runs).toEqual([run]);
	});

	it('listRuns omits the query string when limit is undefined', async () => {
		const fetcher = vi.fn().mockResolvedValue(jsonResponse([]));
		await listRuns(fetcher);
		expect(fetcher).toHaveBeenCalledWith('/api/runs');
	});

	it('getTrace URL-encodes the trace id', async () => {
		const trace: Trace = { trace_id: 'abc', spans: [] };
		const fetcher = vi.fn().mockResolvedValue(jsonResponse(trace));
		await getTrace(fetcher, 'abc/with?weird');
		expect(fetcher).toHaveBeenCalledWith(`/api/traces/${encodeURIComponent('abc/with?weird')}`);
	});

	it('getLogs forwards the traceId query parameter', async () => {
		const logs: LogRecord[] = [];
		const fetcher = vi.fn().mockResolvedValue(jsonResponse(logs));
		await getLogs(fetcher, 'abc');
		expect(fetcher).toHaveBeenCalledWith('/api/logs?traceId=abc');
	});

	it('throws on a non-2xx response and surfaces the error field', async () => {
		const fetcher = vi.fn().mockResolvedValue(
			new Response(JSON.stringify({ error: 'bad trace id' }), {
				status: 400,
				statusText: 'Bad Request',
				headers: { 'Content-Type': 'application/json' }
			})
		);
		await expect(getTrace(fetcher, 'zzz')).rejects.toThrow(/bad trace id/);
	});
});
