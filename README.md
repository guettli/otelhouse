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

## Testing with Dagger's own OTel data

The integration tests (`TestExporter_DaggerLikeTrace`,
`TestMetricExporter_DaggerLikeMetrics`, `TestLogExporter_DaggerLikeLogs`)
create spans, metrics and log records that mirror the attributes Dagger emits
(`dagger.op`, `dagger.cached`, `dagger.cmd`) and verify they land in
ClickHouse.

To feed real Dagger pipeline traces into the exporter, point Dagger at an OTLP
receiver backed by `otelhouse`:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://your-otelhouse-receiver:4318 dagger run ...
```

## Testing == CI

The [Dagger](https://dagger.io/) pipeline in `ci/main.go` is the **single
source of truth** for tests.  Running it locally is byte-identical to what
GitHub Actions runs, so a green local run implies a green CI run:

```sh
make test          # == cd ci && go run .
```

The pipeline stands up its own ephemeral, version-pinned ClickHouse via a
Dagger service binding — there is nothing to install or start by hand, and no
separate local stack to keep in sync.  A reachable Dagger engine is the only
prerequisite; to use a remote engine, export `_EXPERIMENTAL_DAGGER_RUNNER_HOST`
before running.

There is intentionally **no** `docker-compose` (or other) parallel test
environment: a second definition of ClickHouse would drift from `ci/main.go`
and break the "green locally ⇒ green in CI" guarantee.  See
[#33](https://github.com/guettli/otelhouse/issues/33).

The pipeline runs:

1. `gofmt` — format check
2. `go vet` — static analysis
3. `golangci-lint` — lint (`v2.12.2`)
4. `go build` — compilation
5. `go test` — integration tests against a live ClickHouse 25.5 service

## DSN format

```
clickhouse://[user[:password]@]host[:port]/database[?param=value...]
```

`clickhouse://default:@localhost:9000/default` works for a stock
ClickHouse server with no authentication.
