<script lang="ts">
	import Gantt from '$lib/Gantt.svelte';
	import LogViewer from '$lib/LogViewer.svelte';
	import { formatDuration, formatTimestamp, rootSpan, runCommand, statusOf } from '$lib/format';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	const root = $derived(rootSpan(data.trace.spans));
	const command = $derived(runCommand(data.trace.spans));
	const status = $derived(statusOf(root?.status_code ?? ''));

	let selectedSpanId: string | null = $state(null);
	function selectSpan(id: string) {
		selectedSpanId = selectedSpanId === id ? null : id;
	}
</script>

<p class="nav"><a href="/">← All runs</a></p>

<header class="run-head">
	<div>
		<h1 class="mono">{data.trace.trace_id}</h1>
		<p class="cmd">{command || '(no command)'}</p>
	</div>
	<dl class="run-meta">
		<div>
			<dt>Status</dt>
			<dd class="status-{status}">
				{status === 'success' ? 'success' : status === 'error' ? 'error' : 'unknown'}
			</dd>
		</div>
		{#if root}
			<div>
				<dt>Started</dt>
				<dd>{formatTimestamp(root.start_time)}</dd>
			</div>
			<div>
				<dt>Duration</dt>
				<dd>{formatDuration(root.duration_ns)}</dd>
			</div>
		{/if}
		<div>
			<dt>Spans</dt>
			<dd>{data.trace.spans.length}</dd>
		</div>
	</dl>
</header>

<section>
	<h2>Pipeline timeline</h2>
	<p class="hint">Click a span to filter the log viewer below.</p>
	<Gantt spans={data.trace.spans} {selectedSpanId} onSelect={selectSpan} />
</section>

<section>
	<h2>
		Logs
		{#if selectedSpanId}
			<button type="button" class="clear" onclick={() => (selectedSpanId = null)}>
				Show all spans
			</button>
		{/if}
	</h2>
	<LogViewer logs={data.logs} filterSpanId={selectedSpanId} />
</section>

<style>
	.nav {
		margin: 0 0 1rem;
	}
	.run-head {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		gap: 2rem;
		flex-wrap: wrap;
		margin-bottom: 1.5rem;
	}
	h1 {
		font-size: 1rem;
		margin: 0 0 0.25rem;
		color: var(--text-dim);
		font-weight: 500;
	}
	.cmd {
		margin: 0;
		font-size: 1.3rem;
		font-weight: 600;
	}
	.run-meta {
		display: grid;
		grid-auto-flow: column;
		gap: 1.5rem;
		margin: 0;
	}
	.run-meta dt {
		font-size: 0.7rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-dim);
	}
	.run-meta dd {
		margin: 0.15rem 0 0;
		font-size: 0.95rem;
		font-weight: 500;
	}
	section {
		margin-bottom: 2rem;
	}
	h2 {
		font-size: 1.05rem;
		margin: 0 0 0.5rem;
		display: flex;
		align-items: baseline;
		gap: 0.75rem;
	}
	.hint {
		margin: 0 0 0.75rem;
		font-size: 0.85rem;
		color: var(--text-dim);
	}
	.clear {
		background: none;
		border: 1px solid var(--border);
		border-radius: 4px;
		padding: 0.15rem 0.55rem;
		font-size: 0.75rem;
		cursor: pointer;
		color: var(--text-dim);
	}
	.clear:hover {
		background: var(--surface-alt);
	}
</style>
