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
6. **Ingestion-backbone harness** — stands up the upstream
   `otel/opentelemetry-collector-contrib` configured with the
   `clickhouseexporter` (`create_schema: true`) pointed at the same
   ClickHouse service, drives sample OTLP traces/metrics/logs into it with
   `telemetrygen`, and verifies the rows land in the upstream-schema tables
   (`otel_traces`, `otel_logs`, `otel_metrics_gauge`). The Collector config
   lives in [`ci/otel-collector-config.yaml`](ci/otel-collector-config.yaml)
   and mirrors the production deployment minus the bearer-token auth.
