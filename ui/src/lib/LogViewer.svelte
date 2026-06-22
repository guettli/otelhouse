<script lang="ts">
	import { formatTimestamp } from './format';
	import type { LogRecord } from './types';

	let {
		logs,
		filterSpanId = null
	}: {
		logs: LogRecord[];
		filterSpanId?: string | null;
	} = $props();

	const filtered = $derived(
		filterSpanId ? logs.filter((l) => l.span_id === filterSpanId) : logs
	);

	function severityClass(text: string, num: number): string {
		const upper = text.toUpperCase();
		if (upper.includes('ERROR') || upper.includes('FATAL') || num >= 17) return 'err';
		if (upper.includes('WARN') || num >= 13) return 'warn';
		if (upper.includes('DEBUG') || (num > 0 && num <= 8)) return 'dim';
		return '';
	}
</script>

<div class="logs">
	{#if filtered.length === 0}
		<p class="empty">
			{filterSpanId ? 'No log records for this span.' : 'No log records for this trace.'}
		</p>
	{:else}
		<pre class="console">{#each filtered as l (l.timestamp + l.span_id + l.body)}<span class="line {severityClass(l.severity_text, l.severity_number)}"><span class="ts">{formatTimestamp(l.timestamp)}</span> <span class="sev">{l.severity_text || '-'}</span> {l.body}
</span>{/each}</pre>
	{/if}
</div>

<style>
	.logs {
		border: 1px solid var(--border);
		border-radius: 6px;
		background: #0f172a;
		color: #e2e8f0;
		overflow: hidden;
	}
	.console {
		margin: 0;
		padding: 0.75rem 1rem;
		max-height: 24rem;
		overflow: auto;
		font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
		font-size: 0.82rem;
		line-height: 1.45;
		white-space: pre-wrap;
		word-break: break-word;
	}
	.line {
		display: block;
	}
	.ts {
		color: #94a3b8;
	}
	.sev {
		color: #facc15;
	}
	.line.err {
		color: #fca5a5;
	}
	.line.warn {
		color: #fcd34d;
	}
	.line.dim {
		color: #94a3b8;
	}
	.empty {
		padding: 1rem;
		color: var(--text-dim);
		margin: 0;
	}
</style>
