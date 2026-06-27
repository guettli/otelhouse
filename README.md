# otelhouse

OpenTelemetry span, metric and log exporter for [ClickHouse](https://clickhouse.com/).

Spans are stored in a single `otel_traces` table partitioned by day, with
[MergeTree](https://clickhouse.com/docs/en/engines/table-engines/mergetree-family/mergetree)
ordering and a 180-day TTL by default (configurable).  Metrics land in five
`otel_metrics_*` tables — `gauge`, `sum`, `histogram`, `exponential_histogram`
and `summary` — that share the same MergeTree layout and retention.  The
trace and metric schemas intentionally mirror the ones used by the
[OpenTelemetry Collector ClickHouse exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter)
so Grafana dashboards built for that exporter work here too.  Logs land in
`otel_logs` using a schema native to `sdklog.Record` and are *not* drop-in
compatible with the upstream Collector logs table.

## Quick start

```go
import "github.com/guettli/otelhouse"

exp, err := otelhouse.New(ctx, otelhouse.Config{
    DSN: "clickhouse://localhost:9000/default",
})
if err != nil { ... }
defer exp.Shutdown(ctx)

if err := exp.CreateSchema(ctx); err != nil { ... }

tp := sdktrace.NewTracerProvider(
    sdktrace.WithBatcher(exp),
    sdktrace.WithResource(res),
)
```

## Metrics

```go
mexp, err := otelhouse.NewMetricExporter(ctx, otelhouse.MetricConfig{
    DSN: "clickhouse://localhost:9000/default",
})
if err != nil { ... }
defer mexp.Shutdown(ctx)

if err := mexp.CreateSchema(ctx); err != nil { ... }

mp := sdkmetric.NewMeterProvider(
    sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)),
    sdkmetric.WithResource(res),
)
```

`NewMetricExporter` is an
[OTel SDK `metric.Exporter`](https://pkg.go.dev/go.opentelemetry.io/otel/sdk/metric#Exporter):
combine it with a `PeriodicReader` for production, or a `ManualReader` plus
`reader.Collect` for tests.  Each `Export` call fans data points to the
matching `otel_metrics_<gauge|sum|histogram|exponential_histogram|summary>`
table.

## Logs

```go
lexp, err := otelhouse.NewLogExporter(ctx, otelhouse.Config{
    DSN: "clickhouse://localhost:9000/default",
})
if err != nil { ... }
defer lexp.Shutdown(ctx)

if err := lexp.CreateLogSchema(ctx); err != nil { ... }

lp := sdklog.NewLoggerProvider(
    sdklog.WithProcessor(sdklog.NewBatchProcessor(lexp)),
    sdklog.WithResource(res),
)
```

Logs land in `otel_logs` (MergeTree, day-partitioned, 180-day TTL by default).
The schema is native to `sdklog.Record` and is *not* drop-in compatible with
the upstream Collector ClickHouse exporter's logs table.

## Retention

All three schemas set a `TTL` clause of 180 days by default.  Override it on
the config with `RetentionDays`:

```go
otelhouse.Config{DSN: ..., RetentionDays: 30}       // 30-day retention
otelhouse.MetricConfig{DSN: ..., RetentionDays: -1} // no TTL clause emitted
```

A positive value sets that many days; a negative value (e.g. `-1`) omits the
`TTL` clause entirely so retention can be managed out-of-band.  The same
`RetentionDays` field on `Config` is honoured by both `New` (traces) and
`NewLogExporter` (logs).

Retention is baked into the `CREATE TABLE` statement, so changing
`RetentionDays` only affects **newly created** tables — `CREATE TABLE IF NOT
EXISTS` will not modify the TTL of an existing one.  `CreateSchema` /
`CreateLogSchema` logs a warning when it finds the table already present.  To
change retention on an existing table, run `ALTER TABLE … MODIFY TTL …`
yourself in ClickHouse.

## Connection options

`Config` and `MetricConfig` expose a small, typed set of ClickHouse client
tunables that augment the DSN:

```go
otelhouse.Config{
    DSN:          "clickhouse://localhost:9000/default",
    DialTimeout:  5 * time.Second, // default 30s
    ReadTimeout:  30 * time.Second,
    MaxOpenConns: 16,              // 0 → driver default (MaxIdleConns + 5)
    MaxIdleConns: 8,               // 0 → driver default (5)
    Compression:  true,            // enables LZ4
}
```

Zero values mean "use the `clickhouse-go` driver default", so existing
callers see no behavior change.  `Compression: true` enables LZ4; a
`compression=` query parameter on the DSN takes precedence if set.  The same
fields are also available on `MetricConfig` and are honoured by `New`,
`NewMetricExporter` and `NewLogExporter`.

## Local ingestion pipeline (Docker Compose)

A `docker-compose.yml` at the repo root brings up a ready-to-use ingestion
pipeline: a ClickHouse server plus an [OpenTelemetry Collector
contrib](https://github.com/open-telemetry/opentelemetry-collector-contrib)
instance pre-configured with an OTLP receiver and the upstream
`clickhouse` exporter.  The Collector auto-manages the schema (traces,
metrics and logs tables) on first start.

Start the stack:

```sh
docker compose up -d
```

This exposes:

- `localhost:4317` — OTLP gRPC receiver
- `localhost:4318` — OTLP HTTP receiver
- `localhost:8123` — ClickHouse HTTP (user `otel`, password `otel`, db `otel`)
- `localhost:9000` — ClickHouse native TCP

Point any OTLP-aware producer — including Dagger — at the Collector:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 dagger run ...
```

Inspect what landed in ClickHouse with the `clickhouse-client` binary inside
the container:

```sh
docker compose exec clickhouse clickhouse-client \
    --user otel --password otel --database otel \
    --query "SELECT count() FROM otel_traces"
```

Tear the stack down (and drop the data volume) with:

```sh
docker compose down -v
```

The Collector config lives in `otel-collector-config.yaml` — edit it to tune
batching, retention (`ttl`) or to add extra receivers/exporters.

## HTTP API for Dagger traces and logs

Package [`./api`](./api) exposes a read-only HTTP API for querying the data
the Docker Compose pipeline (or this package's own exporters) lands in
ClickHouse.  Three JSON endpoints back the Svelte UI:

- `GET /api/runs` — list of distinct runs (one per `TraceId`) with start time,
  end time, span count, the resource attributes of a representative span, the
  root span's status code and the invoked command (Dagger's `dagger.cmd`
  attribute, falling back to the root span name). Accepts `?limit=N`
  (default `100`, max `1000`).
- `GET /api/traces/{id}` — full span hierarchy for a single trace, ordered by
  start time so the response can be rendered as a Gantt/waterfall without
  re-sorting on the client.
- `GET /api/logs?traceId={id}` — log records carrying the given trace id,
  ordered by timestamp.

The endpoints reference only the columns common to both the upstream
OpenTelemetry Collector ClickHouse exporter and this package's own schemas,
so they work against the Docker Compose stack out of the box.

Run the bundled `otelhouse-api` binary alongside the Compose pipeline:

```sh
go run ./cmd/otelhouse-api \
    -addr :8080 \
    -dsn "clickhouse://otel:otel@localhost:9000/otel"
```

The defaults match the Compose credentials and the standard table names
(`otel_traces`, `otel_logs`); override `-traces-table` / `-logs-table` if
your ingestion pipeline uses different ones.

## Svelte UI for Dagger runs

A SvelteKit single-page app under [`./ui`](./ui) consumes the JSON API
above and renders the data as:

- a dashboard listing past runs with status, timestamp, duration and the
  invoked command;
- a per-run detail page with a Gantt waterfall of the trace's spans and a
  console-style log viewer that can be filtered to a clicked span.

Run it alongside the API in dev:

```sh
go run ./cmd/otelhouse-api -addr :8080 \
    -dsn "clickhouse://otel:otel@localhost:9000/otel" &
cd ui
npm install
npm run dev   # http://localhost:5173, proxies /api -> :8080
```

`npm run build` emits a static bundle under `ui/build/` for production
deployments behind a reverse proxy.

## Testing with Dagger's own OTel data

The integration tests (`TestExporter_DaggerLikeTrace`,
`TestMetricExporter_DaggerLikeMetrics`, `TestLogExporter_DaggerLikeLogs`)
create spans, metrics and log records that mirror the attributes Dagger emits
(`dagger.op`, `dagger.cached`, `dagger.cmd`) and verify they land in
ClickHouse.

To feed real Dagger pipeline traces into the exporter library directly, point
Dagger at an OTLP receiver backed by `otelhouse`:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://your-otelhouse-receiver:4318 dagger run ...
```

For a turnkey local setup, use the Docker Compose stack described above —
it runs the upstream Collector's `clickhouse` exporter, which is schema- and
table-compatible with this library for traces and metrics.

## CI

CI runs through [Dagger](https://dagger.io/) (`ci/main.go`).  The pipeline:

1. `gofmt` — format check
2. `go vet` — static analysis
3. `golangci-lint` — lint (`v2.12.2`)
4. `go build` — compilation
5. `go test` — integration tests against a live ClickHouse 25.5 service

GitHub Actions invokes the pipeline with `go run ./ci/`.

## DSN format

```
clickhouse://[user[:password]@]host[:port]/database[?param=value...]
```

`clickhouse://default:@localhost:9000/default` works for a stock
ClickHouse server with no authentication.
