import { describe, expect, it } from 'vitest';
import {
	formatDuration,
	formatTimestamp,
	isEmptySpanID,
	rootSpan,
	runCommand,
	statusOf
} from '../src/lib/format';
import type { Span } from '../src/lib/types';

const sampleSpan = (overrides: Partial<Span> = {}): Span => ({
	span_id: 'aaaaaaaaaaaaaaaa',
	parent_span_id: '0000000000000000',
	name: 'root',
	kind: 'SPAN_KIND_INTERNAL',
	service_name: 'dagger',
	start_time: '2024-01-01T00:00:00Z',
	duration_ns: 1_000_000_000,
	status_code: '',
	status_message: '',
	span_attributes: {},
	...overrides
});

describe('formatDuration', () => {
	it('renders sub-second values with appropriate units', () => {
		expect(formatDuration(450_000_000)).toBe('450ms');
		expect(formatDuration(750)).toBe('750ns');
		expect(formatDuration(12_500)).toBe('12.5µs');
	});

	it('renders seconds, minutes and hours', () => {
		expect(formatDuration(2_500_000_000)).toBe('2.50s');
		expect(formatDuration(62_000_000_000)).toBe('1m 02s');
		expect(formatDuration(3_700_000_000_000)).toBe('1h 01m');
	});

	it('returns em-dash for negative or non-finite input', () => {
		expect(formatDuration(-1)).toBe('—');
		expect(formatDuration(Number.NaN)).toBe('—');
	});
});

describe('formatTimestamp', () => {
	it('reformats ISO timestamps without fractional seconds', () => {
		expect(formatTimestamp('2024-06-22T12:34:56.789Z')).toBe('2024-06-22 12:34:56Z');
	});
	it('passes invalid strings through unchanged', () => {
		expect(formatTimestamp('not-a-date')).toBe('not-a-date');
	});
});

describe('statusOf', () => {
	it.each([
		['STATUS_CODE_OK', 'success'],
		['Ok', 'success'],
		['STATUS_CODE_ERROR', 'error'],
		['Error', 'error'],
		['', 'unknown'],
		['STATUS_CODE_UNSET', 'unknown']
	])('maps %s to %s', (input, want) => {
		expect(statusOf(input)).toBe(want);
	});
});

describe('isEmptySpanID', () => {
	it('recognises empty and all-zero ids as empty', () => {
		expect(isEmptySpanID('')).toBe(true);
		expect(isEmptySpanID('0000000000000000')).toBe(true);
		expect(isEmptySpanID('abcd1234abcd1234')).toBe(false);
	});
});

describe('rootSpan + runCommand', () => {
	it('picks the parent-less span as root', () => {
		const root = sampleSpan({ span_id: 'r', parent_span_id: '0000000000000000' });
		const child = sampleSpan({
			span_id: 'c',
			parent_span_id: 'r',
			name: 'child',
			start_time: '2024-01-01T00:00:01Z'
		});
		expect(rootSpan([child, root])?.span_id).toBe('r');
	});

	it('prefers the earliest root when multiple exist', () => {
		const a = sampleSpan({ span_id: 'a', start_time: '2024-01-01T00:00:05Z' });
		const b = sampleSpan({ span_id: 'b', start_time: '2024-01-01T00:00:01Z' });
		expect(rootSpan([a, b])?.span_id).toBe('b');
	});

	it('returns dagger.cmd as the command when present', () => {
		const root = sampleSpan({
			name: 'do',
			span_attributes: { 'dagger.cmd': 'dagger call test' }
		});
		expect(runCommand([root])).toBe('dagger call test');
	});

	it('falls back to the root span name when dagger.cmd is missing', () => {
		expect(runCommand([sampleSpan({ name: 'do' })])).toBe('do');
	});
});
