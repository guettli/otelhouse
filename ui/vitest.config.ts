import { defineConfig } from 'vitest/config';

// Vitest config is kept separate from vite.config.ts so the SvelteKit
// build doesn't pull in vitest's bundled Vite copy (which would cause a
// type clash when svelte-check resolves the @sveltejs/kit Vite plugin).
export default defineConfig({
	test: {
		environment: 'jsdom',
		include: ['tests/**/*.{test,spec}.{js,ts}'],
		globals: true
	}
});
