<script lang="ts">
	import { formatDuration, formatTimestamp, statusOf } from '$lib/format';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();
</script>

<h1>Dagger runs</h1>

{#if data.runs.length === 0}
	<p class="empty">No runs found. Send some OTel data to the API and refresh.</p>
{:else}
	<table>
		<thead>
			<tr>
				<th>Status</th>
				<th>Started</th>
				<th>Duration</th>
				<th>Command</th>
				<th>Service</th>
				<th>Spans</th>
				<th>Trace</th>
			</tr>
		</thead>
		<tbody>
			{#each data.runs as run (run.trace_id)}
				{@const status = statusOf(run.status_code)}
				<tr>
					<td>
						<span class="status status-{status}" title={run.status_code || 'unknown'}>
							{status === 'success' ? '✓ success' : status === 'error' ? '✗ error' : '— unknown'}
						</span>
					</td>
					<td>{formatTimestamp(run.start_time)}</td>
					<td>{formatDuration(run.duration_ns)}</td>
					<td class="mono command">{run.command || '—'}</td>
					<td>{run.service_name}</td>
					<td>{run.span_count}</td>
					<td>
						<a class="mono" href="/runs/{run.trace_id}">{run.trace_id.slice(0, 12)}…</a>
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
{/if}

<style>
	h1 {
		font-size: 1.4rem;
		margin: 0 0 1rem;
	}
	.empty {
		color: var(--text-dim);
	}
	.command {
		max-width: 28rem;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.status {
		font-weight: 500;
	}
</style>
