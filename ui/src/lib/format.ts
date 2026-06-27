import type { Span } from './types';

// formatDuration renders a nanosecond count as a short human string.
// Aims to be terse enough for a dense table cell: "12.3s", "450ms", "1m 02s".
export function formatDuration(ns: number): string {
	if (!Number.isFinite(ns) || ns < 0) return '—';
	if (ns < 1_000) return `${ns}ns`;
	if (ns < 1_000_000) return `${(ns / 1_000).toFixed(1)}µs`;
	if (ns < 1_000_000_000) return `${(ns / 1_000_000).toFixed(0)}ms`;
	const seconds = ns / 1_000_000_000;
	if (seconds < 60) return `${seconds.toFixed(2)}s`;
	const minutes = Math.floor(seconds / 60);
	const remSec = Math.floor(seconds - minutes * 60);
	if (minutes < 60) return `${minutes}m ${remSec.toString().padStart(2, '0')}s`;
	const hours = Math.floor(minutes / 60);
	const remMin = minutes - hours * 60;
	return `${hours}h ${remMin.toString().padStart(2, '0')}m`;
}

export function formatTimestamp(iso: string): string {
	const d = new Date(iso);
	if (Number.isNaN(d.getTime())) return iso;
	return d.toISOString().replace('T', ' ').replace(/\.\d+Z$/, 'Z');
}

// runStatus returns "success", "error" or "unknown" from a span's StatusCode.
// OTel reports "STATUS_CODE_OK", "STATUS_CODE_ERROR" or empty/unset; the API
// passes the raw enum value through.
export function statusOf(code: string): 'success' | 'error' | 'unknown' {
	const c = code.toUpperCase();
	if (c.includes('OK')) return 'success';
	if (c.includes('ERROR')) return 'error';
	return 'unknown';
}

// rootSpan picks the root of the trace: the span whose parent_span_id is
// missing or all zeros. If multiple roots exist (multi-trace edge case) the
// earliest-starting one wins.
export function rootSpan(spans: Span[]): Span | undefined {
	const roots = spans.filter((s) => isEmptySpanID(s.parent_span_id));
	if (roots.length === 0) return undefined;
	return roots.reduce((earliest, s) =>
		new Date(s.start_time).getTime() < new Date(earliest.start_time).getTime() ? s : earliest
	);
}

export function isEmptySpanID(id: string): boolean {
	return id === '' || /^0+$/.test(id);
}

// Dagger's CLI records the invoked command in a span attribute called
// `dagger.cmd`. Surface it when present, fall back to the root span name.
export function runCommand(spans: Span[]): string {
	const root = rootSpan(spans);
	if (!root) return '';
	return root.span_attributes['dagger.cmd'] ?? root.name;
}
