# otelhouse

OpenTelemetry → [ClickHouse](https://clickhouse.com/) ingestion harness.

Ingestion is **codeless**: the upstream
[OpenTelemetry Collector ClickHouse exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter)
owns the schema and writes traces, metrics and logs directly. This
repository hosts the Dagger-driven harness that stands up the Collector
alongside a ClickHouse service and validates the pipeline end-to-end.

The in-process Go exporter that previously lived here has been removed —
it re-implemented the upstream exporter and was a maintenance liability.
See [#29](https://github.com/guettli/otelhouse/issues/29) for the
architecture overview and [#32](https://github.com/guettli/otelhouse/issues/32)
for the epic.

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
6. **End-to-end harness** — stands up the upstream
   `otel/opentelemetry-collector-contrib` (with
   [`ci/otel-collector-config.yaml`](ci/otel-collector-config.yaml))
   pointed at the same ClickHouse service, drives sample OTLP
   traces/metrics/logs into it with `telemetrygen`, runs the
   `otelhouse-api` binary as a Dagger service against ClickHouse, and
   runs the `TestE2E_API` Go test (build tag `e2e`) which hits
   `/api/runs`, `/api/traces/:id` and `/api/logs?traceId=:id` and asserts
   the API renders the ingested data. This is the
   `Dagger → OTLP → Collector → ClickHouse → API` guarantee for the whole
   harness — one pipeline run validates everything end-to-end.

## Connecting traces and logs

The upstream `clickhouseexporter` writes `TraceId` and `SpanId` columns to
both `otel_traces` and `otel_logs`, so a log emitted inside an active span
joins back to that span with no custom schema:

```sql
SELECT t.SpanName, l.Body
FROM otel_traces  t
JOIN otel_logs    l USING (TraceId, SpanId)
```

For the join to work, producers must emit log records while a span is
active — start the span (`tracer.Start(ctx, ...)`) before the log call so
the OTel SDK stamps the span context onto the record. A log with an empty
`SpanId` cannot be linked to a span, and a Collector pipeline that strips
`TraceId`/`SpanId` (e.g. via `attributes/delete`) breaks the join.

This is the data foundation the API ([#26](https://github.com/guettli/otelhouse/issues/26))
and visualization UI ([#27](https://github.com/guettli/otelhouse/issues/27))
build on to render hyperlinks between a span and its logs (and back).
