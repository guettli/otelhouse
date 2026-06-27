// The UI runs as a static SPA: SvelteKit prerenders the shell and the API
// is hit client-side. Disable SSR so we don't try to call the API from
// the build process.
export const ssr = false;
export const prerender = true;
