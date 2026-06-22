# otelhouse UI

A SvelteKit single-page app that visualises Dagger CI runs stored by
[otelhouse](../README.md). It fetches data from the Go HTTP API in
[`cmd/otelhouse-api`](../cmd/otelhouse-api) and renders:

- a dashboard listing past runs (`/`)
- a per-run detail page with a Gantt waterfall of spans and a console-style
  log viewer (`/runs/<trace-id>`)

## Develop

```sh
cd ui
npm install
npm run dev
```

The dev server runs at <http://localhost:5173> and proxies `/api/*` to
`http://localhost:8080` (override with `OTELHOUSE_API_URL`). Start the API
in another terminal:

```sh
go run ./cmd/otelhouse-api -addr :8080 \
    -dsn "clickhouse://otel:otel@localhost:9000/otel"
```

## Build a static bundle

```sh
npm run build
```

The build is emitted under `ui/build/`. Serve it with any static file
server — for example behind the same reverse proxy that fronts the API,
so `/api/*` and the SPA are co-located.

## Tests

```sh
npm test            # vitest unit tests
npm run check       # svelte-check / TS type-check
```
