import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

// The Svelte UI talks to the Go API server (cmd/otelhouse-api) at /api/*.
// In dev, proxy those calls through Vite so the SvelteKit dev server can
// serve the UI on :5173 while the API listens on :8080.
export default defineConfig({
	plugins: [sveltekit()],
	server: {
		proxy: {
			'/api': {
				target: process.env.OTELHOUSE_API_URL ?? 'http://localhost:8080',
				changeOrigin: true
			}
		}
	}
});
