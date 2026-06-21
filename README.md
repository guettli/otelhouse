# otelhouse

OpenTelemetry span exporter for [ClickHouse](https://clickhouse.com/).

Spans are stored in a single `otel_traces` table partitioned by day, with
[MergeTree](https://clickhouse.com/docs/en/engines/table-engines/mergetree-family/mergetree)
ordering and a 180-day TTL.  The schema intentionally mirrors the one used by
the [OpenTelemetry Collector ClickHouse exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter)
so Grafana dashboards built for that exporter work here too.

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

## Testing with Dagger's own OTel data

The integration test (`TestExporter_DaggerLikeTrace`) creates spans that mirror
the attributes Dagger emits (`dagger.op`, `dagger.cached`, `dagger.cmd`) and
verifies they land in ClickHouse.

To feed real Dagger pipeline traces into the exporter, point Dagger at an OTLP
receiver backed by `otelhouse`:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://your-otelhouse-receiver:4318 dagger run ...
```

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
