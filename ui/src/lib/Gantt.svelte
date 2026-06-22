<script lang="ts">
	import { formatDuration } from './format';
	import { statusOf } from './format';
	import type { Span } from './types';

	let {
		spans,
		selectedSpanId = null,
		onSelect
	}: {
		spans: Span[];
		selectedSpanId?: string | null;
		onSelect?: (spanID: string) => void;
	} = $props();

	// startMs[i] is the unix-ms start time of spans[i]; precompute so we
	// don't pay for new Date() inside the layout pass for every span.
	const startMs = $derived(spans.map((s) => new Date(s.start_time).getTime()));
	const traceStartMs = $derived(startMs.length ? Math.min(...startMs) : 0);
	const traceEndMs = $derived(
		spans.length
			? Math.max(...spans.map((s, i) => startMs[i] + s.duration_ns / 1_000_000))
			: 0
	);
	const traceDurationMs = $derived(Math.max(traceEndMs - traceStartMs, 1));

	// Nested hierarchy so we can indent children by depth. A span whose
	// parent isn't in `spans` is treated as a top-level row.
	type Row = { span: Span; depth: number; index: number };
	const rows = $derived(layout(spans));

	function layout(input: Span[]): Row[] {
		const byParent = new Map<string, Span[]>();
		const ids = new Set(input.map((s) => s.span_id));
		for (const s of input) {
			const parent = ids.has(s.parent_span_id) ? s.parent_span_id : '';
			if (!byParent.has(parent)) byParent.set(parent, []);
			byParent.get(parent)!.push(s);
		}
		for (const list of byParent.values()) {
			list.sort((a, b) => new Date(a.start_time).getTime() - new Date(b.start_time).getTime());
		}
		const out: Row[] = [];
		const walk = (parent: string, depth: number) => {
			for (const s of byParent.get(parent) ?? []) {
				out.push({ span: s, depth, index: input.indexOf(s) });
				walk(s.span_id, depth + 1);
			}
		};
		walk('', 0);
		return out;
	}

	function offsetPct(row: Row): number {
		const ms = startMs[row.index] - traceStartMs;
		return (ms / traceDurationMs) * 100;
	}
	function widthPct(row: Row): number {
		const ms = row.span.duration_ns / 1_000_000;
		// Keep tiny bars visible.
		return Math.max((ms / traceDurationMs) * 100, 0.4);
	}
</script>

<div class="gantt">
	{#each rows as row (row.span.span_id)}
		{@const status = statusOf(row.span.status_code)}
		<button
			type="button"
			class="row"
			class:selected={selectedSpanId === row.span.span_id}
			onclick={() => onSelect?.(row.span.span_id)}
			title="{row.span.name} — {formatDuration(row.span.duration_ns)}"
		>
			<div class="label" style="padding-left: {row.depth * 1.1}rem">
				<span class="name">{row.span.name}</span>
				<span class="meta mono">{formatDuration(row.span.duration_ns)}</span>
			</div>
			<div class="track">
				<div
					class="bar status-bar-{status}"
					style="left: {offsetPct(row)}%; width: {widthPct(row)}%"
				></div>
			</div>
		</button>
	{/each}
</div>

<style>
	.gantt {
		display: flex;
		flex-direction: column;
		gap: 1px;
		background: var(--border);
		border: 1px solid var(--border);
		border-radius: 6px;
		overflow: hidden;
	}
	.row {
		display: grid;
		grid-template-columns: 22rem 1fr;
		align-items: center;
		gap: 0.5rem;
		padding: 0.35rem 0.5rem;
		background: var(--surface);
		border: none;
		text-align: left;
		font: inherit;
		color: inherit;
		cursor: pointer;
	}
	.row:hover {
		background: var(--surface-alt);
	}
	.row.selected {
		background: #eef4ff;
	}
	.label {
		display: flex;
		justify-content: space-between;
		gap: 0.5rem;
		min-width: 0;
	}
	.name {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: 0.85rem;
	}
	.meta {
		color: var(--text-dim);
		flex-shrink: 0;
	}
	.track {
		position: relative;
		height: 14px;
		background: var(--surface-alt);
		border-radius: 3px;
	}
	.bar {
		position: absolute;
		top: 0;
		bottom: 0;
		background: var(--bar);
		border-radius: 3px;
	}
	.status-bar-error {
		background: var(--bar-error);
	}
	.status-bar-success {
		background: var(--bar);
	}
	.status-bar-unknown {
		background: var(--bar);
	}
</style>
