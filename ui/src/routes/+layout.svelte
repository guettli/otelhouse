<script lang="ts">
	import '../app.css';
	let { children } = $props();
</script>

<header>
	<a class="brand" href="/">otelhouse</a>
	<span class="tagline">Dagger CI runs</span>
</header>

<main>
	{@render children()}
</main>

<style>
	header {
		display: flex;
		align-items: baseline;
		gap: 1rem;
		padding: 0.75rem 1.5rem;
		border-bottom: 1px solid var(--border);
		background: var(--surface);
	}
	.brand {
		font-weight: 600;
		font-size: 1.1rem;
		color: var(--text);
		text-decoration: none;
	}
	.tagline {
		color: var(--text-dim);
		font-size: 0.9rem;
	}
	main {
		padding: 1.5rem;
		max-width: 1200px;
		margin: 0 auto;
	}
</style>
